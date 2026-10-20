# Sentrix: Go Backend Spec

Owner: Go service (MQTT ingestion + REST API + alert engine + Python trigger)

---

## 1. Role of the Go service

Go is the glue between the ESP32 fleet, the database, the Python ML service, and the Next.js frontend.

| Responsibility | Direction |
|---|---|
| Subscribe to MQTT broker, ingest handshake + telemetry | ESP32 → broker → **Go** |
| Insert telemetry into Postgres (Supabase) | Go → DB |
| Day 8 check: flip CALIBRATING → ACTIVE, trigger training | Go → Python |
| Check each reading against `prediction_bounds`, create alerts | Go → DB |
| Send Web Push for new alerts | Go → browser |
| REST API for Next.js (groups, devices, telemetry, bounds, alerts) | Next.js → **Go** |

**Key points**
- The ESP32 never calls Go. Go is just another client of the MQTT broker.
- Next.js never calls Python. Python is only reachable from Go.
- The database is the meeting point between services.

---

## 2. Architecture
Browser (Next.js) ──REST (JWT)──► Go API ──► Supabase Postgres ◄── Python (writes prediction_bounds)
▲ ▲ │
│ Supabase Realtime │ └── POST /api/calibrate (Day 8) ──► Python FastAPI
│ (telemetry, alerts) │
│ MQTT subscribe
ESP32 root node ──MQTTS:8883──► MQTT Broker (HiveMQ Cloud / Mosquitto)


Services: Python (ML), Next.js (web), Go (this doc).

---

## 3. MQTT contract (give this to the IoT team)

### Broker
- Host/port: `xxxx.s1.eu.hivemq.cloud:8883` (TLS)
- One device credential (username/password) for ESP32s, and a separate credential for the Go service.
- Library: `github.com/eclipse/paho.mqtt.golang` with auto-reconnect and persistent subscription.

### Topics (DECISION NEEDED, docs currently conflict)

Your notes use two different schemes:

| Source | Handshake | Telemetry |
|---|---|---|
| Firmware flowchart | `sentrix/auth/{mac}/connect` | `sentrix/dt/{meshId}/batch` |
| Earlier notes | `sentrix/handshake` | `sentrix/telemetry/{mac}` |

**Recommendation:** follow the firmware flowchart, since the root node batches the whole mesh:

- Handshake: `sentrix/auth/{mac}/connect`. Go subscribes to `sentrix/auth/+/connect`.
- Telemetry: `sentrix/dt/{meshId}/batch`. Go subscribes to `sentrix/dt/+/batch`.

Pick one scheme, then write it in a shared doc so IoT and Go match exactly.

### Handshake payload (ESP32 → Go)

```json
{
  "mac_address": "AA:BB:CC:DD:EE:FF",
  "device_pin": "123456",
  "mesh_id": "MESH_89AB",
  "role": "root",
  "firmware_version": "1.0.0",
  "timestamp": 1729584000
}
```

Every node (root and leaf) sends one. Leaves route theirs through the root.

### Batch telemetry payload (proposed, confirm with IoT)

The firmware design has the root poll children every 1 minute and publish one batch:

```json
{
  "mesh_id": "MESH_89AB",
  "timestamp": 1729584060,
  "readings": [
    { "mac_address": "AA:BB:CC:DD:EE:FF", "rms_current": 5.2, "online": true },
    { "mac_address": "11:22:33:44:55:66", "rms_current": 0.0, "online": false }
  ]
}
```

- Timeout nodes (no response within 50ms) should be flagged `online: false` rather than silently dropped.
- Timestamp is the NTP-synced Unix time from the ESP32. Store it as the reading's `timestamp`.

---

## 4. Database (relevant parts of the Prisma schema)

| Table | Key columns | Go usage |
|---|---|---|
| `users` | id, email, role (ADMIN/USER) | Owner check on groups |
| `groups` | id, user_id, name, **mesh_id (unique)**, description | Create + list |
| `devices` | id, **mac_address (unique)**, pin_code, name, group_id, status, calibration_started_at, calibration_ends_at | Claim, handshake, Day 8 check |
| `telemetry` | id (BigInt), device_id, timestamp, rms_current | Bulk insert every minute |
| `prediction_bounds` | id, device_id, timestamp, upper_bound, lower_bound | Read for alert check + charts |
| `alerts` | id, device_id, timestamp, actual_value, expected_max, expected_min, severity, is_resolved, user_feedback | Create, resolve, RL feedback |

Enums:
- `DeviceStatus`: UNCLAIMED, CALIBRATING, ACTIVE, OFFLINE
- `AlertSeverity`: WARNING, CRITICAL
- `Role`: ADMIN, USER

Indexes already defined: `[device_id, timestamp DESC]` on telemetry, prediction_bounds and alerts. Use these for time-range queries.

Go should use `pgx` (or `sqlc`) directly rather than Prisma. Run `prisma migrate` once so the DB exists.

---

## 5. Flows Go is responsible for

### Flow 1: Onboarding

1. User clicks **Create Group**. Next.js calls `POST /api/groups`. Go generates a unique 6-byte `mesh_id` and saves it, linked to the user.
2. User clicks **Add Device**. Next.js sends `{ssid, password, pin, mesh_id}` to the ESP32 over Web Bluetooth (no Go involvement).
3. Next.js calls `POST /api/groups/{id}/devices` with `{mac, pin}`. Go:
   - verifies the group belongs to the JWT user
   - verifies the PIN against `devices.pin_code`
   - sets `group_id`
   - sets `status = CALIBRATING`
   - sets `calibration_started_at = now()` and `calibration_ends_at = now() + 7 days`
4. ESP32 joins Wi-Fi and mesh, then publishes the handshake. Go verifies MAC + PIN and marks the device online.

### Flow 2: Telemetry (every minute)

On each batch message Go does:
1. Parse JSON and validate `mesh_id` and MACs.
2. Bulk `INSERT` readings into `telemetry`.
3. Run the **Day 8 check** (Flow 3) for each device.
4. Run the **alert check** (section 6) for each device that is ACTIVE.

For 3 nodes a plain insert is fine. Move to batched inserts / `COPY` if volume grows.

### Flow 3: Day 8 trigger (Go → Python)

For each device in the batch:
if status == CALIBRATING && now() > calibration_ends_at:
1. UPDATE devices SET status = ‘ACTIVE’ WHERE id = ? AND status = ‘CALIBRATING’
(do this FIRST, and check rows affected, so training is never triggered twice)
2. POST http://python-service:8000/api/calibrate
Header: X-Internal-Key: <shared secret>
Body: {“device_id”: “...”}
3. Python returns 202 immediately and trains in the background
4. Python writes prediction_bounds to the DB


Notes:
- Use an atomic `UPDATE ... WHERE status = 'CALIBRATING'` and only call Python when 1 row was updated. This also protects against concurrent messages.
- If the Python call fails, log it and retry, or set the status back so it can be retried.
- Python endpoint naming conflict: docs use both `/mulai-training` and `/api/calibrate`. **Use `/api/calibrate`.**

### Flow 4: Dashboard data

Next.js gets chart data from Go REST (or Prisma server-side). Live updates come from Supabase Realtime on `telemetry` and `alerts` INSERT events, which does not involve Go.

### Flow 5: Alerts and push

- A new row in `alerts` triggers the in-app toast through Realtime automatically.
- For Web Push (tab closed): store the user's push subscription in the DB, sign with VAPID keys, and have Go send the push when it creates the alert.

---

## 6. Alert logic (recommended: Go owns it)

The docs say Python creates alerts, but because bounds are pre-computed, the check is trivial and cheaper in Go:
bound = SELECT upper_bound, lower_bound
FROM prediction_bounds
WHERE device_id = ? AND timestamp = date_trunc(‘hour’, reading.timestamp)
if reading < lower_bound OR reading > upper_bound:
INSERT INTO alerts (device_id, timestamp, actual_value, expected_max, expected_min, severity)


Python stays training/forecasting only.

Considerations:
- **Hourly bounds vs 1-minute data:** ARIMA is trained on hourly averages, so the same bound applies to all 60 minutes. Expect noise. Require **3 consecutive breaches** before alerting.
- **Deduplicate:** don't create a new alert every minute while the same anomaly continues.
- **Severity:** e.g. WARNING for a small breach, CRITICAL if it exceeds the bound by some margin (define threshold).
- **State gating:** skip alert checks if current is below the standby threshold (e.g. < 0.1 A).

---

## 7. Bounds expiry

Python only forecasts 24 hours ahead, so after about Day 9 there are no bounds. Fix:
- Add a daily cron (in Go or Python) that re-runs the forecast for every ACTIVE device.
- Forecast 48 hours ahead as a buffer.
- In Go, if no bound exists for the current hour, skip the alert check (do not crash or alert).

---

## 8. REST API (for Next.js, not the ESP32)
POST /api/groups create group + mesh_id
GET /api/groups list my groups
POST /api/groups/{id}/devices claim device (mac, pin)
GET /api/devices/{id}/telemetry?range=1h|24h|7d
GET /api/devices/{id}/bounds?range=24h
GET /api/alerts?status=unresolved
PATCH /api/alerts/{id}/feedback body: true | false (RL feedback)
PATCH /api/alerts/{id}/resolve


Likely additions for the UI:
- `GET /api/groups/{id}` for group detail + device table
- `GET /api/dashboard/summary` for active/offline nodes, anomalies in last 24h/7d
- `POST /api/push/subscribe` to save the Web Push subscription

### Auth
- Frontend uses Supabase Auth and sends the JWT in `Authorization: Bearer <jwt>`.
- Go verifies the JWT with the Supabase JWT secret.
- **Every endpoint must check ownership** (the group/device/alert belongs to the JWT user). Admins can bypass.

### Internal endpoint protection
- Python's `/api/calibrate` is protected by a shared secret header `X-Internal-Key`. It should not be reachable from the public internet or from Next.js.

---

## 9. Environment variables
DATABASE_URL= # Supabase Postgres connection string
SUPABASE_JWT_SECRET= # verify frontend JWTs
MQTT_BROKER_URL= # tls://xxxx.s1.eu.hivemq.cloud:8883
MQTT_USERNAME=
MQTT_PASSWORD=
PYTHON_SERVICE_URL= # http://python-service:8000
INTERNAL_API_KEY= # sent as X-Internal-Key
CALIBRATION_MINUTES=10080 # 7 days; set to 5 for testing
VAPID_PUBLIC_KEY=
VAPID_PRIVATE_KEY=
PORT=8080


Don't commit real keys. Keep a `.env.example` only.

---

## 10. Suggested build order

1. Run `prisma migrate` so the DB exists.
2. Create the HiveMQ Cloud broker, then send host, credentials and topic format to the IoT team.
3. Build the Go MQTT subscriber that inserts into `telemetry`. Test with **MQTTX** or `mosquitto_pub` as a fake ESP32.
4. Build the Python FastAPI `/api/calibrate` (test with fake data).
5. Add the Go **Day 8 check**, with `CALIBRATION_MINUTES=5` for testing.
6. Add the Go **alert check** against `prediction_bounds`.
7. Build the REST endpoints, then connect the Next.js pages.
8. Add Web Push last.

Fake-ESP32 test example:

```bash
mosquitto_pub -h <host> -p 8883 -u <user> -P <pass> --cafile ca.pem \
  -t "sentrix/dt/MESH_89AB/batch" \
  -m '{"mesh_id":"MESH_89AB","timestamp":1729584060,"readings":[{"mac_address":"AA:BB:CC:DD:EE:FF","rms_current":5.2,"online":true}]}'
```

---

## 11. Open decisions / gaps to settle with the team

1. **Topic scheme** (section 3): `sentrix/dt/{mesh}/batch` vs `sentrix/telemetry/{mac}`.
2. **Batch payload schema** with the IoT team (section 3).
3. **Who creates alerts:** recommended Go (section 6).
4. **Bounds expiry job:** who runs the daily forecast cron.
5. **Consecutive-breach rule** (3 in a row?) and severity thresholds.
6. **Offline detection:** how Go marks a device OFFLINE (e.g. no data for N minutes, or `online: false` in batch).
7. **Device pre-registration:** devices must exist in `devices` as UNCLAIMED with MAC + PIN before claim. Decide how they're seeded (manual script for the MVP).
8. **Per-device broker permissions** (later): restrict topics by MAC.
9. **Possible scope change:** the AC use-case notes (IR target temperature, mode, fan speed, outdoor temp) would add fields to the payload and `telemetry` table. Decide if this is in the MVP.

---

## 12. Quick reference: who talks to whom

| From | To | How | When |
|---|---|---|---|
| Browser | ESP32 | Web Bluetooth | Once, at onboarding |
| ESP32 root | Broker | MQTTS 8883 | Handshake + every 1 min |
| Go | Broker | MQTT subscribe | Always connected |
| Go | Postgres | INSERT / UPDATE | Every message |
| Go | Python | HTTP POST `/api/calibrate` | Day 8, once per device |
| Python | Postgres | Bulk upsert `prediction_bounds` | After training + daily |
| Browser | Go | REST + JWT | Dashboard use |
| Postgres | Browser | Supabase Realtime | Live chart + alert toasts |