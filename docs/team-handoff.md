# Sentrix Go backend: status and what each team needs to know

Written 2026-10-20 for the IoT, frontend and ML teams. This describes what the Go service (`sentrix-api`) does **today**, not the plan. For the full design see `sentrix-architecture.md`; for every REST route in detail see `api.md`.

Contents: [1. Status](#1-status) · [2. For the IoT team](#2-for-the-iot-team) · [3. For the frontend team](#3-for-the-frontend-team) · [4. For the ML (Python) team](#4-for-the-ml-python-team) · [5. Open decisions](#5-open-decisions) · [6. Running and testing locally](#6-running-and-testing-locally) · [7. Configuration](#7-configuration) · [8. Known limitations](#8-known-limitations)

---

## 1. Status

### Done

| Area | What exists |
|---|---|
| **MQTT ingest** | Subscribes to the IoT team's two topics, `sentrix/auth/+/connect` (handshake) and `sentrix/dt/+/batch` (data recording), QoS 1, persistent session, auto-reconnect. Validates every message, stores each reading with its temperatures in `telemetry`, and caches device lookups. |
| **Handshake** | Verifies MAC + PIN (and `mesh_id` once the device is in a group), then saves `last_seen`, `firmware_version`, `local_ip`, `wifi_rssi` and `role`. |
| **Offline detection** | Nodes listed in a batch's `offline_nodes` become `OFFLINE`; a node that reports again or sends a handshake comes back (to `ACTIVE` if bounds exist, else `CALIBRATING`). |
| **Day 8 calibration trigger** | When a `CALIBRATING` device's `calibration_ends_at` has passed, Go flips it to `ACTIVE` (atomically, once) and calls the ML service `POST /api/calibrate`. If the call fails it reverts to `CALIBRATING` so the next reading retries. |
| **Alert engine** | For each reading from an `ACTIVE` device, compares it to the hourly predicted bound. Alerts after 3 consecutive breaches (rules in section 8). |
| **Web Push** | Sends a push notification to the device owner's subscribed browsers when an alert is created. |
| **REST API** | Health, auth check, groups, device claim, device list, telemetry and bounds for charts, group detail, dashboard summary, alert feed + feedback + resolve, push subscribe/unsubscribe. All behind Supabase login. |
| **Security** | Supabase JWT verification (ES256 or HS256), ownership checks on every route, PIN rate limiting on device claim, CORS allow-list, push endpoint allow-list. |
| **Tooling** | Simulator (`cmd/simulator`), VAPID key generator (`cmd/vapidgen`), CI (`go vet`, `go build`, `go test`), unit tests, and integration tests that run against a dev database. |

### Not done

| Area | State | Blocked on |
|---|---|---|
| **Daily bounds refresh** | No job re-runs the forecast, so bounds run out ~48h after calibration. | ML service |
| **ML service** | `sentrix-ml` repo is empty. Nothing writes `prediction_bounds`, so no real alerts can happen yet. | ML team |
| **Admin bypass** | The `ADMIN` role is not treated specially; every user only sees their own data. | Decision whether it is needed |
| **Handshake reply** | Go never answers the ESP32, so a node cannot tell if its handshake was accepted (a rejected one is only logged). | Decision if the firmware needs a reply topic |
| **Per-device broker permissions** | All ESP32s would share one broker credential. | Broker set-up |
| **Deployment** | Runs on a laptop only. No Dockerfile, no hosted broker, no domain/HTTPS. See `going-live.md`. | — |
| **Frontend wiring** | The dashboard still shows hard-coded mock data and never calls this API. | Frontend team |

---

## 2. For the IoT team

Go implements **your format** exactly as you sent it. The full description, every field, test commands (copy-paste) and a reference ESP-IDF program are in **`esp32-mqtt-examples.md`**. Summary:

**Broker**: the ESP32 needs a broker host/port (TLS, port 8883), a username and a password. These do not exist yet; we will create the broker and send them to you. Use QoS 1.

| Topic | Who publishes | When | Purpose |
|---|---|---|---|
| `sentrix/auth/{MAC}/connect` | **every** node (children via the root) | after each connect | handshake: proves identity, reports firmware/IP/RSSI |
| `sentrix/dt/{mesh_id}/batch` | the **root** only | every minute | current (and temperatures) of every node, plus `offline_nodes` |

`{MAC}` is written with colons (`AA:BB:CC:DD:EE:00`). Go also accepts lowercase and `-`, and stores it as `AABBCCDDEE00`.

**Handshake** fields: `mac_address`, `device_pin` (a 6-digit **string**), `mesh_id`, `role` (`root`/`child`), `firmware_version`, `local_ip`, `wifi_rssi`, `timestamp`.

**Batch** fields: `mesh_id`, `timestamp` (applies to every reading), `root_mac`, `readings[]` with `mac`, `node_type`, `rms_current`, `current_temperature`, `target_temperature`, and `offline_nodes[]`.

### Rules Go applies

- **`mesh_id`** must equal the one in the topic. It must also be the `mesh_id` the web app wrote to the ESP32 over Bluetooth, which Go generated when the user created the group (**12 uppercase hex characters**, not `MESH_89AB`; that is only an example). A reading for a device whose group has a different `mesh_id` is dropped.
- **`timestamp`** is Unix **seconds** from NTP. Missing, zero or negative drops the whole batch. Readings older than 10 minutes are stored but never raise alerts.
- **`rms_current`** is required on every reading (amps, 0 or more). Write `0.00` for a stopped machine. A missing value skips that reading; it is never stored as 0.
- One bad reading does not drop the others. The same device and `timestamp` is stored once, so a retry is safe.
- Readings from a device that is not claimed (`UNCLAIMED`) are silently ignored. A MAC that is not in the database is logged and dropped.
- A current under **0.1 A** is stored but treated as "machine off": never an alert.
- **`offline_nodes`**: those devices become `OFFLINE` (only if they belong to this `mesh_id`). They return to service automatically the next time they report or send a handshake.
- **Go never replies** on MQTT. Success is visible only in the database and the service log.

### Onboarding order (what the user does)

1. Web app creates a group; Go returns a unique `mesh_id`.
2. The web app writes `{ssid, password, pin, mesh_id}` to the ESP32 over Web Bluetooth.
3. The web app calls the claim endpoint with the device MAC and PIN; the device becomes `CALIBRATING` (7 days by default).
4. The ESP32 joins Wi-Fi/mesh, sends its handshake, and starts publishing batches.

Every device must already exist in the `devices` table with its MAC and a 6-digit PIN **before** a user can claim it. For now these rows are inserted by hand (the repo's `supabase/seed.sql` shows one). Decide who does this for real devices (section 5).

### Test it without hardware

```
mosquitto_pub -t "sentrix/dt/MESH_89AB/batch" -q 1 -m '{"mesh_id":"MESH_89AB","timestamp":'$(date +%s)',"root_mac":"AA:BB:CC:DD:EE:00","readings":[{"mac":"AA:BB:CC:DD:EE:00","node_type":"root","rms_current":5.2}],"offline_nodes":[]}'
```

Or run the simulator, which sends the same handshake and batch messages: `go run ./cmd/simulator -backfill-days 1`.

---

## 3. For the frontend team

### Connecting

| | |
|---|---|
| Base URL (dev) | `http://localhost:8080` → set `NEXT_PUBLIC_GO_API_URL` |
| Auth | Header `Authorization: Bearer <supabase access_token>` on every route except `/health` |
| Get the token | `const { data } = await supabase.auth.getSession(); data.session?.access_token` |
| CORS | The server allows origins listed in its `ALLOWED_ORIGINS` (default `http://localhost:3000`). Tell us your deployed origin. |
| Errors | JSON `{"error": "message"}`. `401` bad/missing token, `404` not found **or not yours**, `400` invalid input |

Quick check that your token works: `GET /api/me` → `{"user_id": "..."}`.

### Endpoints (full shapes and status codes: `api.md`)

| Method | Path | Use |
|---|---|---|
| GET | `/health` | public liveness check |
| GET | `/api/me` | confirm login works |
| POST | `/api/groups` | create group `{name, description?}` → returns `mesh_id` |
| GET | `/api/groups` | list my groups |
| GET | `/api/groups/{id}` | group detail |
| POST | `/api/groups/{id}/devices` | **claim a device** `{mac, pin, name?}` |
| GET | `/api/groups/{id}/devices` | devices in a group (never includes the PIN) |
| GET | `/api/devices/{id}/telemetry?range=1h\|24h\|7d` | chart points (`1h` raw; `24h` 5-min buckets; `7d` 15-min buckets, each with average and peak) |
| GET | `/api/devices/{id}/bounds?range=1h\|24h\|7d` | predicted safe zone, hourly, including future hours |
| GET | `/api/dashboard/summary` | counts: groups, devices by status, 24h anomalies, unresolved alerts |
| GET | `/api/alerts` | alert feed; filters `status`, `group`, `device`, `reviewed`, `limit`, `before` |
| PATCH | `/api/alerts/{id}/feedback` | `{"user_feedback": true\|false}` (true = real anomaly) |
| PATCH | `/api/alerts/{id}/resolve` | mark resolved |
| POST | `/api/push/subscribe` | register this browser for push notifications |
| DELETE | `/api/push/subscribe` | unregister `{endpoint}` |

IDs in paths are UUIDs. Someone else's resource and a nonexistent one both return `404`.

### Things to know

- **The dashboard currently uses mock data** (`initialDevices`, `initialAlerts`, a fixed SVG chart). Nothing calls this API yet. A device has no "health %" or "kW"; the real data is **amps per circuit** (`rms_current`), with bounds and alerts.
- **Onboarding calls, in order:** `POST /api/groups` (use the returned `mesh_id`) → write `{ssid, password, pin, mesh_id}` to the ESP32 over BLE → `POST /api/groups/{id}/devices`. Claim errors: `404` means wrong MAC/PIN or already claimed (deliberately not distinguished), `429` means too many wrong PINs (see `Retry-After`).
- **Device status** values: `UNCLAIMED`, `CALIBRATING`, `ACTIVE`, `OFFLINE`. `OFFLINE` is set when the root reports the node in `offline_nodes`, and clears itself when the node reports again. Each device object also has `last_seen`, `firmware_version`, `local_ip`, `wifi_rssi` and `node_role` (null until the device's first handshake). Use `last_seen` for "seen 2 minutes ago".
- **Bounds are empty until calibration finishes** and the ML service writes them. Draw the chart without the shaded band when `bounds` is `[]`.
- **Live updates** come from Supabase Realtime (`INSERT` on `telemetry` and `alerts`), not from this API. Realtime and row-level security are set up in the migrations. Use `/api/alerts` and `/telemetry` for the first load.
- **Do not read the `devices` table directly with the Supabase client.** The owner's row-level-security policy would expose `pin_code`. Use the API, which never returns it.
- **Web Push needs frontend work**: a service worker and a subscribe call. Copy-paste code is in `api.md` under "Web Push". Needs HTTPS (or `localhost`) and `NEXT_PUBLIC_VAPID_PUBLIC_KEY`.
- **Web Bluetooth** needs Chrome/Edge (desktop) or Chrome on Android, over HTTPS or `localhost`.
- Env vars the frontend uses for this: `NEXT_PUBLIC_SUPABASE_URL`, `NEXT_PUBLIC_SUPABASE_ANON_KEY`, `NEXT_PUBLIC_GO_API_URL`, `NEXT_PUBLIC_VAPID_PUBLIC_KEY`.

---

## 4. For the ML (Python) team

Go calls Python in one place. Nothing else calls it, and the browser never does.

```
POST {PYTHON_SERVICE_URL}/api/calibrate
Header:  X-Internal-Key: <INTERNAL_API_KEY>
Body:    {"device_id": "<device uuid>"}
Expect:  HTTP 202 immediately (training runs in the background)
```

- Any status other than `202`, or a timeout (10 s), counts as failure: Go sets the device back to `CALIBRATING` and tries again on its next reading (about every minute), so the endpoint must be safe to call repeatedly. Return `202` quickly.
- Go marks the device `ACTIVE` **before** calling you.
- Python reads `telemetry` directly from Postgres (aggregate to hourly in SQL) and **upserts into `prediction_bounds`**. The table has `unique (device_id, "timestamp")`, `lower_bound <= upper_bound`, and one row per hour; the `timestamp` must be the **top of the hour** (Go looks up `timestamp = hour of the reading`, exact match). Write the **current** hour and forecast **48 hours** ahead.
- Go skips alerting for any hour that has no bound row.
- Bounds expire. A daily job must re-run the forecast for every `ACTIVE` device. This job does not exist yet; decide whether Go or Python owns it (section 5).

---

## 5. Open decisions

| # | Decision | Who | Options / recommendation |
|---|---|---|---|
| 1 | ~~MQTT topics and payload~~ | IoT + Go | **Decided: the IoT team's format** (`sentrix/auth/{mac}/connect`, `sentrix/dt/{meshId}/batch`). Implemented in Go. |
| 2 | ~~Single vs batched telemetry~~ | IoT | **Decided: one batch per mesh per minute**, from the root. |
| 3 | ~~How offline is detected~~ | IoT + Go | **Decided: the root lists non-answering nodes in `offline_nodes`.** A node is not marked offline merely for being quiet. |
| 4 | Who seeds `devices` rows (MAC + PIN) for real hardware | everyone | Manual script for the MVP. |
| 5 | Who runs the daily bounds refresh | Go + ML | Go cron calling Python, or a Python scheduler. |
| 6 | Alert tuning | Go + ML | Currently 3 consecutive breaches, 30-minute cooldown, standby under 0.1 A, "critical" beyond one band-width. Adjust after seeing real data. |
| 7 | Admin role | team | Should `ADMIN` see all groups? Not built. |
| 8 | ML approach | team | Architecture assumes Auto-ARIMA on the server; the sprint doc mentions Isolation Forest / edge inference. |
| 9 | BLE payload size | IoT + frontend | Whether `{ssid, password, pin, mesh_id}` needs chunking. |
| 10 | AC-specific fields | team | `current_temperature` and `target_temperature` are stored per reading. Mode and fan speed are not in the format and are not stored. The REST API does not return the temperatures yet (ask the frontend team when they need them). |

---

## 6. Running and testing locally

Requirements: Go, a Supabase project with the migrations applied, an MQTT broker (Mosquitto for local).

```bash
brew install mosquitto          # needs /opt/homebrew/etc/mosquitto/mosquitto.conf with:
                                #   listener 1883 127.0.0.1
                                #   allow_anonymous true
brew services start mosquitto

cp .env.example .env            # fill DATABASE_URL (Supabase pooler string), SUPABASE_URL, ...
set -a; source .env; set +a
go run ./cmd/api                # MQTT ingest + HTTP API on :8080
```

- **`DATABASE_URL`**: use Supabase's **pooler** string (IPv4); the direct `db.<ref>.supabase.co` host is IPv6-only and fails on many networks. Remove a trailing `?pgbouncer=true` (that is a Prisma option).
- **Simulator** (fake ESP32): `go run ./cmd/simulator -backfill-days 1` sends the handshake and a day of history as batches; `-live -interval 5s -spike-every 20` keeps sending and injects 15 A spikes. Defaults to the local Mosquitto, device `AABBCCDDEEFF` (sent as `AA:BB:CC:DD:EE:FF`) and `-mesh MESH_89AB`; flags `-mac`, `-pin`, `-mesh`.
- **Quick calibration test:** set `CALIBRATION_MINUTES=5` before claiming a device.
- **Tests:** `go test ./...`. Integration tests that touch the real dev database: `go test -tags integration ./...` (they create throwaway rows and delete them; never run against production).
- **Only run one copy of the API.** It uses a fixed MQTT client ID, so a second copy will fight the first for the connection. If messages stop arriving after copies were left running, reconnect once with a clean session: `mosquitto_sub -i sentrix-go-backend -t zz/none -W 1`.
- **Database migration:** the new device fields and the temperature columns need `supabase/migrations/20261020010000_device_presence_and_temperature.sql` (in `sentrix-web`) applied to your Supabase project, or ingest will fail on every reading.
- The dev database currently contains a seed device `AABBCCDDEEFF` (PIN `123456`) and simulator test data.

---

## 7. Configuration

| Variable | Required | Meaning |
|---|---|---|
| `DATABASE_URL` | yes | Supabase pooler connection string |
| `MQTT_BROKER_URL` | yes | e.g. `tcp://localhost:1883` or `tls://host:8883` |
| `MQTT_USERNAME`, `MQTT_PASSWORD` | for a real broker | |
| `PYTHON_SERVICE_URL`, `INTERNAL_API_KEY` | for calibration | the ML service and the shared secret |
| `SUPABASE_URL` | yes for login to work | project URL; tokens are verified against its public keys (ES256/RS256) |
| `SUPABASE_JWT_SECRET` | only legacy projects | shared secret for HS256 tokens |
| `PORT` | no (8080) | HTTP port |
| `ALLOWED_ORIGINS` | no (`http://localhost:3000`) | comma-separated browser origins |
| `CALIBRATION_MINUTES` | no (10080 = 7 days) | calibration length after a claim |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`, `VAPID_SUBJECT` | for Web Push | generate keys once with `go run ./cmd/vapidgen -env .env`; subject is a `mailto:` or `https:` contact. Never change the keys later: every browser subscription would stop working. |

Secrets live only in `.env` (git-ignored) and on the host; never in the repo or chat.

---

## 8. Known limitations

- **Alert rules** (`internal/ingest/alerts.go`): ignore readings under 0.1 A and readings older than 10 minutes; skip hours with no bound; alert on the **3rd consecutive** reading outside the bound, once per run; at least 30 minutes between alerts for a device; severity `CRITICAL` when more than one band-width outside the bound, else `WARNING`.
- **State is in memory**: the breach counters, the PIN rate limiter and the device/bound caches reset when the service restarts, and the service must run as a **single instance**.
- **Device cache**: status changes made directly in the database can take up to 15 seconds to be noticed by ingest.
- **PIN rate limit**: 5 failed claims per user and 20 per device MAC per 15 minutes.
- **Messages sent while Go is down** are delivered when it reconnects (persistent session), up to the broker's queue limit (Mosquitto default 1000).
- **Ingest speed**: the simulator publishes faster than a single Go instance writes if sent with no delay; use its default pause for long backfills.
- **Push endpoints** are limited to Google, Mozilla, Apple and Windows push services, and 20 per user.
