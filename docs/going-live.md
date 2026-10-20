# sentrix-api: Going Live Checklist

Moving from local testing (Mosquitto + fake `mosquitto_pub` messages) to real ESP32 devices.
The Go ingest code, topics, JSON payloads, MAC format and tables stay the same. What changes is config, deployment, and a few unfinished features.
Contracts: see `sentrix-architecture.md` section 5.

## 1. Build the missing features first

Real onboarding does not work without these (see `handoff.md` known gaps).

- [ ] Handshake handling: verify MAC + PIN, mark device online (needs a `last_seen` column or similar).
- [ ] Device claim endpoint (`POST /api/groups/{id}/devices`): checks PIN, sets `CALIBRATING`, sets `calibration_ends_at` from `CALIBRATION_MINUTES` (7 days = 10080 in production).
- [ ] REST API with Supabase JWT check, ownership check, and PIN rate limiting.
- [ ] Bounds check and alerts (3 consecutive breaches or a cooldown).
- [ ] Unique constraint on `telemetry(device_id, timestamp)` so redelivered QoS 1 messages are not stored twice (architecture section 7).
- [ ] Decide how devices go `OFFLINE` and back (architecture gotcha #13).

## 2. MQTT broker (HiveMQ Cloud or similar)

- [ ] Create the cluster and credentials.
- [ ] Set in Go's environment: `MQTT_BROKER_URL=tls://<host>:8883`, `MQTT_USERNAME`, `MQTT_PASSWORD` (Go's own credential, subscribe permission).
- [ ] Send the architecture section 5.1 table to the IoT team: host and port, device username/password, topics, QoS 1.
- [ ] Later: per-device credentials restricted to publishing on `sentrix/telemetry/{their mac}`.
- [ ] Only one Go instance per MQTT client ID (`sentrix-go-backend`); a second instance disconnects the first.

## 3. Deploy the Go service

- [ ] Host that runs 24/7 (readings published while Go is down are only delivered after it reconnects, via the persistent session).
- [ ] Set environment variables on the host, not in a committed file: `DATABASE_URL`, `MQTT_*`, `PYTHON_SERVICE_URL`, `INTERNAL_API_KEY`, `CALIBRATION_MINUTES`, plus `SUPABASE_URL`, `ALLOWED_ORIGINS`, `PORT`, and `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`, `VAPID_SUBJECT` (`SUPABASE_JWT_SECRET` only for a legacy HS256 project).
- [ ] `DATABASE_URL` uses the Supabase pooler string (the app already uses the simple protocol for it).
- [ ] The service exits cleanly on SIGTERM, so it is safe under a process manager or container.

## 4. Python service

- [ ] Deploy `sentrix-ml` somewhere Go can reach but the public internet cannot (private network or firewall).
- [ ] `PYTHON_SERVICE_URL` points at it.
- [ ] `INTERNAL_API_KEY`: replace `dev-secret` with a long random value, identical in Go and Python.
- [ ] Confirm `/api/calibrate` returns 202; until then Go reverts devices to `CALIBRATING` on every message after day 7.

## 5. Database

- [ ] Decide: keep using the dev Supabase project, or create a separate production project and apply all migrations there.
- [ ] Remove test data: `delete from telemetry;` and reset or delete the seed device `AABBCCDDEEFF` (otherwise the model trains on fake data).
- [ ] Realtime enabled for `telemetry` and `alerts`, RLS policies in place (already in the initial migration; verify in the dashboard).
- [ ] Real devices need `devices` rows with their MAC and PIN (printed on the sticker) before they can be claimed.

## 6. Frontend (`sentrix-web`)

- [ ] Set `NEXT_PUBLIC_GO_API_URL` to the deployed Go API once the REST endpoints exist.
- [ ] Web Bluetooth only works over HTTPS in Chrome/Edge or Chrome on Android.
- [ ] Web Push: generate VAPID keys once with `go run ./cmd/vapidgen`, public key in the frontend env (`NEXT_PUBLIC_VAPID_PUBLIC_KEY`), private key only in Go's env, `VAPID_SUBJECT` set to a real contact. Reuse the same keys everywhere: changing them invalidates every browser subscription.

## 7. Firmware (IoT team)

- [ ] Publishes to `sentrix/handshake` and `sentrix/telemetry/{MAC}` with QoS 1, over TLS.
- [ ] MAC is uppercase, 12 hex characters, no colons. In a mesh, the MAC is the originating node's, not the root's.
- [ ] Payload includes a valid `ts` (Unix seconds, from NTP); Go drops readings with a missing `ts`.
- [ ] Single reading: topic MAC must equal payload MAC, or Go drops it. Batches are exempt.
- [ ] Agree single vs batched format (open decision).

## 8. End-to-end test before the demo

- [ ] One real device: onboard via the dashboard, see the handshake succeed, see telemetry rows arrive every minute.
- [ ] Shorten calibration (e.g. `CALIBRATION_MINUTES=5`) and confirm Go triggers Python and `prediction_bounds` rows appear.
- [ ] Trigger a load spike and confirm an alert appears (dashboard toast, and Web Push with the tab closed).
- [ ] Unplug Wi-Fi or power and confirm the device recovers and no duplicate rows appear.
- [ ] Never commit `.env` or paste secrets into chat.
