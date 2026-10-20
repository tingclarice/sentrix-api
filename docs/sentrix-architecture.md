# Sentrix: System Integration Guide

Predictive maintenance and energy monitoring: ESP32 + SCT-013 sensors → MQTT → Go → Postgres (Supabase) → Python (Auto-ARIMA) → Next.js dashboard.

> **Audience:** full-stack devs (Next.js / Go / Python) and the IoT team.
> **Purpose:** one place that explains how the 3 services and the ESP32 connect, who owns what, and what to build in which order.

---

## Table of Contents

1. [Project Context](#1-project-context)
2. [The Big Idea (Read This First)](#2-the-big-idea-read-this-first)
3. [System Architecture](#3-system-architecture)
4. [Service Responsibilities](#4-service-responsibilities)
5. [Contracts Between Components](#5-contracts-between-components)
6. [End-to-End Flows](#6-end-to-end-flows)
7. [Database Schema](#7-database-schema)
8. [Code Reference](#8-code-reference)
9. [Auth & Security](#9-auth--security)
10. [Known Gotchas & Required Fixes](#10-known-gotchas--required-fixes)
11. [Environment Variables](#11-environment-variables)
12. [Build Order](#12-build-order)
13. [Testing Without Hardware](#13-testing-without-hardware)
14. [Demo Checklist](#14-demo-checklist)
15. [Appendix: UI Routes, Hardware, Sprint Plan](#15-appendix)

---

## 1. Project Context

**Sentrix** is an AI-powered electrical monitoring and predictive maintenance system for factories, commercial buildings, and facility managers.

- **Problem:** unexpected electrical equipment failures cause downtime, repair costs, and inefficiency.
- **Sensing:** SCT-013 split-core CT clamps around the MCB wire. An ESP32 reads it through its ADC and computes RMS current every minute.
- **AI:** Auto-ARIMA learns the normal current rhythm of each circuit over 7 days, then flags readings outside the dynamic confidence interval.
- **Granularity:** monitoring is per MCB/circuit group, **not** per appliance.
- **Business model:** one-time installation fee + monthly software subscription.
- **First customers:** Universitas Ciputra, Ciputra World, manufacturing companies in Surabaya.

### Tech stack

| Layer | Tech |
|---|---|
| Edge | ESP32, SCT-013, ESP-Mesh / ESP-Mesh-Lite, NimBLE, EmonLib-style RMS |
| Messaging | MQTT (HiveMQ Cloud or Mosquitto) over TLS |
| Backend API | Go (`eclipse/paho.mqtt.golang`, REST) |
| ML | Python (FastAPI, `pmdarima`, pandas) |
| Database | Supabase (PostgreSQL), SQL migrations in `sentrix-web/supabase/migrations` |
| Frontend | Next.js, Recharts, Supabase Auth, Supabase Realtime, Web Bluetooth, Web Push |

---

## 2. The Big Idea (Read This First)

**The services do not talk to each other directly. They meet through two shared things:**

1. **The MQTT broker** is the mailbox between the ESP32 and Go.
2. **The Postgres database** is the shared memory between Go, Python, and Next.js.

Rules to remember:

- **The ESP32 never calls Go.** It publishes to the MQTT broker. Go is just another client subscribed to that broker.
- **Next.js never calls Python.** It reads results from the database. Python is an internal service only Go calls.
- **Python never runs when a user opens the dashboard.** It pre-computes the "safe zone" bounds and stores them. Next.js only does a fast `SELECT`.
- **Bluetooth is used exactly once per device**, at onboarding, to hand Wi-Fi credentials to the ESP32.

### Analogy: MQTT is a WhatsApp group

| WhatsApp | MQTT |
|---|---|
| The group | Broker (HiveMQ / Mosquitto) |
| Group name | Topic (`sentrix/telemetry/...`) |
| Sending a message | Publish |
| Joining the group | Subscribe |

The ESP32 posts to the group and Go sits in the same group and reads every message. Neither needs the other's IP address.

---

## 3. System Architecture

```
                      (one time only, Web Bluetooth)
  Browser (Next.js) ──────────────────────────────────► ESP32
        │  ▲                                              │
        │  │ REST + JWT          Supabase Realtime        │ MQTT publish (TLS)
        ▼  │                     (WebSocket, live)        ▼
   ┌─────────┐                                    ┌─────────────┐
   │  Go API │◄───── MQTT subscribe ──────────────│ MQTT Broker │
   └────┬────┘                                    │ HiveMQ /    │
        │ INSERT telemetry                        │ Mosquitto   │
        │ UPDATE device status                    └─────────────┘
        │ INSERT alerts
        ▼
   ┌──────────────────────┐
   │ Supabase Postgres    │◄──── Python reads telemetry,
   │  users, groups       │      writes prediction_bounds
   │  devices             │
   │  telemetry           │
   │  prediction_bounds   │
   │  alerts              │
   └──────────────────────┘
        ▲
        │ POST /api/calibrate (Day 8 only, Go → Python, internal)
   ┌─────────────┐
   │ Python      │
   │ FastAPI     │
   └─────────────┘
```

### Who talks to whom

| From | To | Protocol | When |
|---|---|---|---|
| Next.js (browser) | ESP32 | Web Bluetooth (BLE GATT) | Once, at onboarding |
| ESP32 | MQTT broker | MQTT over TLS | Handshake + every 1 min |
| MQTT broker | Go | MQTT subscribe | Every message |
| Go | Postgres | SQL | Every message |
| Go | Python | HTTP POST (internal) | Day 8, once per device |
| Python | Postgres | SQL | Calibration + daily forecast refresh |
| Next.js | Go | REST + JWT | Page loads, actions |
| Next.js | Postgres | Supabase Realtime (WebSocket) | Live chart and alert updates |
| Go | Browser | Web Push | Alert while tab is closed |

---

## 4. Service Responsibilities

### Go (the hub)
- Subscribes to MQTT (`handshake` and `telemetry`).
- Verifies device MAC + PIN on handshake and marks the device online.
- Inserts every telemetry reading into `telemetry`.
- Runs the Day 8 check and triggers Python.
- **(Recommended)** Checks each reading against `prediction_bounds` and inserts into `alerts`.
- Sends Web Push notifications.
- Exposes REST endpoints to Next.js (groups, devices, telemetry, bounds, alerts).

### Python (training only)
- FastAPI service with `POST /api/calibrate`.
- Fits `auto_arima` on 7 days of hourly-averaged data.
- Forecasts the next N hours with confidence intervals and bulk-upserts into `prediction_bounds`.
- Daily cron: refreshes bounds for all `ACTIVE` devices.
- **Does not** serve the website or talk to the browser.

### Next.js (UI)
- Auth via Supabase Auth.
- Onboarding modal using Web Bluetooth.
- Dashboard charts (Recharts): `telemetry` line + `prediction_bounds` shaded area + `alerts` red markers.
- Live updates via Supabase Realtime.
- Alerts page with True/False feedback buttons (human-in-the-loop).
- Registers the Web Push service worker.

### ESP32 (IoT team)
- BLE onboarding server (NimBLE), stores credentials in NVS.
- ESP-Mesh root election, NTP time sync.
- Computes RMS every minute and publishes to MQTT.

---

## 5. Contracts Between Components

> **Lock these down in the first meeting.** Mismatched names are the #1 source of integration bugs.

### 5.1 What the Go dev hands to the IoT team

The IoT team does **not** need a Go endpoint. They need broker details:

| Item | Example |
|---|---|
| Broker host + port | `xxxx.s1.eu.hivemq.cloud:8883` (TLS) |
| Username / password | Device credential (store in ESP32 NVS or firmware config) |
| Handshake topic | `sentrix/auth/{mac}/connect` |
| Data recording topic | `sentrix/dt/{mesh_id}/batch` |
| QoS | 1 |

### 5.2 MAC format

The IoT team writes MACs with colons (`AA:BB:CC:DD:EE:00`) in topics and payloads. Go normalizes every MAC (any case, `:` or `-`) to **uppercase, 12 hex characters, no colons** (`AABBCCDDEE00`) before it touches the database.

### 5.3 MQTT topics (decided: the IoT team's format)

| Topic | Publisher | Subscriber | Purpose |
|---|---|---|---|
| `sentrix/auth/{mac}/connect` | every ESP32 node (children via the root) | Go (`sentrix/auth/+/connect`) | Handshake: prove identity, report firmware/IP/RSSI |
| `sentrix/dt/{mesh_id}/batch` | the mesh root | Go (`sentrix/dt/+/batch`) | Data recording, once a minute for the whole mesh |

**Mesh note:** only the root node has internet. It publishes the batch for the whole mesh; each reading carries its own node's MAC.

### 5.4 Payloads

The complete, current definition (every field, validation rules, what Go does with each case) is in **`esp32-mqtt-examples.md`**. In short:

Handshake: `mac_address`, `device_pin` (6-digit string), `mesh_id`, `role`, `firmware_version`, `local_ip`, `wifi_rssi`, `timestamp`.

Data recording: `mesh_id`, `timestamp` (Unix seconds, applies to all readings), `root_mac`, `readings[]` (`mac`, `node_type`, `rms_current`, `current_temperature`, `target_temperature`) and `offline_nodes[]`.

The `mesh_id` is the 12-hex-character value Go generates when a group is created and the web app writes to the ESP32 over Bluetooth. Go drops readings whose `mesh_id` does not match the device's group.

### 5.5 Go REST API (for Next.js)

All routes require `Authorization: Bearer <supabase_jwt>`.

```
POST  /api/groups                          create group + generate 6-byte mesh_id
GET   /api/groups                          list my groups
POST  /api/groups/{id}/devices             claim device {mac, pin}
GET   /api/groups/{id}/devices             devices in group
GET   /api/devices/{id}/telemetry?range=1h|24h|7d
GET   /api/devices/{id}/bounds?range=24h
GET   /api/alerts?status=unresolved&group=...
PATCH /api/alerts/{id}/feedback            {"user_feedback": true|false}
PATCH /api/alerts/{id}/resolve
POST  /api/push/subscribe                  save Web Push subscription
```

### 5.6 Python internal API (only Go calls this)

```
POST /api/calibrate
Header: X-Internal-Key: <shared secret>
Body:   {"device_id": "<device uuid>"}
→ 202 {"status": "calibration_started", "device_id": "..."}
```
Pick **one** path name. The notes currently mention both `/mulai-training` and `/api/calibrate`; use `/api/calibrate`.

---

## 6. End-to-End Flows

### Flow 1: Onboarding (Next.js → ESP32 → Go)

1. User logs in (Supabase Auth).
2. User clicks **Create Group** → Next.js `POST /api/groups` → Go generates a unique 6-byte `mesh_id` and saves it.
3. User clicks **Add Device** → Next.js calls `navigator.bluetooth.requestDevice` filtered by the custom service UUID.
4. User picks the device (matches MAC sticker), enters the 6-digit PIN, confirms Wi-Fi SSID/password (auto-filled from `localStorage`).
5. Next.js writes `{ssid, password, pin, mesh_id}` to the ESP32 BLE GATT characteristic.
6. Next.js also calls `POST /api/groups/{id}/devices` with `{mac, pin}`. Go then:
   - verifies the PIN against `devices.pin_code`
   - sets `group_id`
   - sets `status = CALIBRATING`
   - sets `calibration_started_at = now()`, `calibration_ends_at = now() + 7 days`
7. ESP32 saves credentials to NVS, shuts down BLE, starts mesh, elects root, syncs NTP.
8. Root publishes handshake → Go verifies MAC+PIN → marks device online.

### Flow 2: Telemetry (every minute)

```
ESP32 → publish sentrix/telemetry/{mac} → Broker → Go handler:
   1. parse JSON
   2. look up device by MAC
   3. INSERT into telemetry
   4. (ACTIVE devices) check bounds → INSERT alert if breached
   5. (CALIBRATING devices) Day 8 check
```

### Flow 3: Day 8 trigger (Go → Python)

Go does **not** count rows. It just looks at the clock:

```
IF status == CALIBRATING AND now() > calibration_ends_at:
    1. Set status = ACTIVE (atomically, so it can't fire twice)
    2. POST python:8000/api/calibrate {"device_id": ...}
```
Python returns 202 immediately and runs `auto_arima` in a background task, then writes `prediction_bounds`.

### Flow 4: Dashboard

- **Initial chart load:** Next.js → Go REST (or Prisma server-side) → query `telemetry` + `prediction_bounds` + `alerts` for the selected timeframe.
- **Live updates:** browser subscribes to Supabase Realtime `INSERT` events on `telemetry` and `alerts`.
- Next.js **never** requests data from Python.

### Flow 5: Alerts and push

- **Dashboard open:** Realtime pushes the new `alerts` row → show toast / bell badge.
- **Tab closed:** Web Push. Needs a service worker, VAPID keys, and each user's push subscription saved in the DB. Go sends the push when it inserts the alert.
- **User feedback:** True/False buttons on the Alerts page → `PATCH /api/alerts/{id}/feedback`. This is stored for later model tuning.

---

## 7. Database Schema

Source of truth is the SQL migrations in `sentrix-web/supabase/migrations` (`20261014000000_initial_schema.sql`). Summary:

```
User 1──* Group 1──* Device 1──* Telemetry
                          ├────* PredictionBound
                          └────* Alert
```

| Table | Key columns |
|---|---|
| `users` | id, email, name, role (`ADMIN`/`USER`) |
| `groups` | id, user_id, name, mesh_id (unique), description |
| `devices` | id, mac_address (unique), pin_code, name, group_id, status, calibration_started_at, calibration_ends_at |
| `telemetry` | id (bigint), device_id, timestamp, rms_current |
| `prediction_bounds` | id (bigint), device_id, timestamp, upper_bound, lower_bound |
| `alerts` | id, device_id, timestamp, actual_value, expected_max, expected_min, severity, is_resolved, user_feedback |

`DeviceStatus`: `UNCLAIMED → CALIBRATING → ACTIVE` (and `OFFLINE`).
`alerts.user_feedback`: `null` = unreviewed, `true` = real anomaly, `false` = false alarm.

### Recommended schema changes

Added as `sentrix-web/supabase/migrations/20261018000000_telemetry_unique_device_timestamp.sql`:

```sql
-- prediction_bounds already has unique (device_id, "timestamp") in the initial migration.

-- Lets Go use ON CONFLICT DO NOTHING so a redelivered QoS 1 message isn't stored twice
alter table telemetry add constraint telemetry_device_ts_key unique (device_id, "timestamp");
```

`push_subscriptions` (endpoint unique, p256dh, auth, user_id) already exists in the initial migration. A `last_seen` column on `devices` is still needed to mark devices online/offline from handshakes.

---

## 8. Code Reference

### 8.1 Go: MQTT subscriber

```go
package main

import (
    "encoding/json"
    "log"
    "os"
    "time"

    mqtt "github.com/eclipse/paho.mqtt.golang"
)

type TelemetryMsg struct {
    MAC        string  `json:"mac"`
    RMSCurrent float64 `json:"rms_current"`
    TS         int64   `json:"ts"`
}

func newMQTTClient() mqtt.Client {
    opts := mqtt.NewClientOptions().
        AddBroker(os.Getenv("MQTT_BROKER_URL")). // tls://host:8883
        SetUsername(os.Getenv("MQTT_USERNAME")).
        SetPassword(os.Getenv("MQTT_PASSWORD")).
        SetClientID("sentrix-go-backend").
        SetAutoReconnect(true).
        SetConnectRetry(true).
        SetKeepAlive(30 * time.Second).
        // Re-subscribe on every (re)connect, otherwise subscriptions are lost after a drop
        SetOnConnectHandler(func(c mqtt.Client) {
            c.Subscribe("sentrix/handshake", 1, handleHandshake)
            c.Subscribe("sentrix/telemetry/+", 1, handleTelemetry)
            log.Println("MQTT connected and subscribed")
        }).
        SetConnectionLostHandler(func(_ mqtt.Client, err error) {
            log.Printf("MQTT connection lost: %v", err)
        })

    client := mqtt.NewClient(opts)
    if t := client.Connect(); t.Wait() && t.Error() != nil {
        log.Fatal(t.Error())
    }
    return client
}

func handleTelemetry(_ mqtt.Client, msg mqtt.Message) {
    var m TelemetryMsg
    if err := json.Unmarshal(msg.Payload(), &m); err != nil {
        log.Printf("bad telemetry payload: %v", err)
        return
    }
    // 1. device := lookupDeviceByMAC(m.MAC)  (ignore unknown / UNCLAIMED)
    // 2. insertTelemetry(device.ID, time.Unix(m.TS, 0), m.RMSCurrent)
    // 3. if device.Status == "CALIBRATING" { maybeStartCalibration(device.ID) }
    // 4. if device.Status == "ACTIVE"      { checkBoundsAndAlert(device.ID, m) }
}
```

### 8.2 Go: Day 8 check (idempotent)

The single `UPDATE ... WHERE status='CALIBRATING'` is atomic. Only one message can win it, so Python can never be triggered twice.

```go
func maybeStartCalibration(ctx context.Context, db *pgxpool.Pool, deviceID string) {
    tag, err := db.Exec(ctx, `
        UPDATE devices
        SET status = 'ACTIVE'
        WHERE id = $1
          AND status = 'CALIBRATING'
          AND calibration_ends_at <= now()`, deviceID)
    if err != nil || tag.RowsAffected() == 0 {
        return // not yet 7 days, or another message already won
    }

    go func() {
        if err := triggerPython(deviceID); err != nil {
            log.Printf("python trigger failed for %s: %v, reverting", deviceID, err)
            // Revert so the next message retries
            db.Exec(context.Background(),
                `UPDATE devices SET status='CALIBRATING' WHERE id=$1`, deviceID)
        }
    }()
}

func triggerPython(deviceID string) error {
    body, _ := json.Marshal(map[string]string{"device_id": deviceID})
    req, _ := http.NewRequest("POST", os.Getenv("PYTHON_SERVICE_URL")+"/api/calibrate", bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-Internal-Key", os.Getenv("INTERNAL_API_KEY"))

    resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
    if err != nil { return err }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusAccepted {
        return fmt.Errorf("python returned %d", resp.StatusCode)
    }
    return nil
}
```

### 8.3 Go: bounds check and alert (recommended location for alert logic)

```go
func checkBoundsAndAlert(ctx context.Context, db *pgxpool.Pool, deviceID string, ts time.Time, value float64) {
    var lower, upper float64
    err := db.QueryRow(ctx, `
        SELECT lower_bound, upper_bound FROM prediction_bounds
        WHERE device_id = $1 AND timestamp = date_trunc('hour', $2::timestamptz)`,
        deviceID, ts).Scan(&lower, &upper)
    if err != nil {
        return // no bounds for this hour yet; skip (see Gotcha #3)
    }
    if value >= lower && value <= upper {
        return
    }
    // TODO: require N consecutive breaches / dedupe within X minutes before inserting
    db.Exec(ctx, `
        INSERT INTO alerts (device_id, timestamp, actual_value, expected_max, expected_min, severity)
        VALUES ($1, $2, $3, $4, $5, 'WARNING')`,
        deviceID, ts, value, upper, lower)
    // then: send Web Push to the group owner's subscriptions
}
```

### 8.4 Python: FastAPI trigger

```python
import os
from fastapi import FastAPI, BackgroundTasks, Header, HTTPException

app = FastAPI()

def verify_key(x_internal_key: str | None):
    if x_internal_key != os.getenv("INTERNAL_API_KEY"):
        raise HTTPException(status_code=401, detail="unauthorized")

@app.post("/api/calibrate", status_code=202)
async def trigger_calibration(
    data: dict,
    background_tasks: BackgroundTasks,
    x_internal_key: str | None = Header(default=None),
):
    verify_key(x_internal_key)
    device_id = data["device_id"]
    background_tasks.add_task(calibrate_device, device_id)
    return {"status": "calibration_started", "device_id": device_id}
```

### 8.5 Python: fit and bulk-insert bounds

Query Postgres directly (psycopg / SQLAlchemy) and aggregate to hourly **in SQL**. This avoids the Supabase client's default row limit (see Gotcha #2).

```python
import pandas as pd
from pmdarima import auto_arima

def calibrate_device(device_id: str, horizon: int = 48):
    # 1. Hourly averages for the last 7 days, aggregated in SQL
    df = pd.read_sql("""
        SELECT date_trunc('hour', timestamp) AS ts, avg(rms_current) AS rms
        FROM telemetry
        WHERE device_id = %(id)s
          AND timestamp >= now() - interval '7 days'
          AND timestamp <  date_trunc('hour', now())   -- exclude the incomplete current hour
        GROUP BY 1 ORDER BY 1
    """, engine, params={"id": device_id})

    if len(df) < 48:
        print(f"{device_id}: not enough data"); return

    ts = df.set_index("ts")["rms"].asfreq("h").interpolate()

    # 2. Fit
    model = auto_arima(ts, seasonal=True, m=24, start_p=1, max_p=3, start_q=1, max_q=3,
                       d=None, stepwise=True, suppress_warnings=True, error_action="ignore")

    # 3. Forecast with confidence interval
    forecast, conf_int = model.predict(n_periods=horizon, return_conf_int=True)
    future = pd.date_range(ts.index[-1] + pd.Timedelta(hours=1), periods=horizon, freq="h")

    rows = [{"device_id": device_id, "timestamp": future[i].isoformat(),
             "lower_bound": float(conf_int[i][0]), "upper_bound": float(conf_int[i][1])}
            for i in range(horizon)]

    # 4. Single bulk upsert (requires UNIQUE(device_id, timestamp))
    upsert_bounds(rows)
```

### 8.6 Next.js: Web Bluetooth

```bash
npm install --save-dev @types/web-bluetooth
```

```ts
const SERVICE_UUID = "12345678-1234-5678-1234-56789abcdef0";

const device = await navigator.bluetooth.requestDevice({
  filters: [{ services: [SERVICE_UUID] }], // only shows Sentrix hardware
});
const server = await device.gatt!.connect();
const service = await server.getPrimaryService(SERVICE_UUID);
const characteristic = await service.getCharacteristic(CONFIG_CHAR_UUID); // agree with IoT team

const payload = JSON.stringify({ ssid, password, pin, mesh_id });
await characteristic.writeValue(new TextEncoder().encode(payload));
```
Requires HTTPS (or `localhost`). BLE writes are limited by MTU, so ask the IoT team whether the payload needs chunking.

### 8.7 Next.js: live updates (Supabase Realtime)

```ts
supabase
  .channel("device-" + deviceId)
  .on("postgres_changes",
      { event: "INSERT", schema: "public", table: "telemetry", filter: `device_id=eq.${deviceId}` },
      (payload) => addPointToChart(payload.new))
  .on("postgres_changes",
      { event: "INSERT", schema: "public", table: "alerts", filter: `device_id=eq.${deviceId}` },
      (payload) => showAlertToast(payload.new))
  .subscribe();
```
You must enable Realtime for `telemetry` and `alerts` in the Supabase dashboard and add RLS policies so users only receive rows for their own devices.

---

## 9. Auth & Security

| Link | Mechanism |
|---|---|
| Browser → Go | Supabase Auth JWT in `Authorization` header; Go verifies with the Supabase JWT secret |
| Go ownership check | On every group/device/alert route, verify the resource belongs to the JWT's user |
| Go → Python | Shared secret `X-Internal-Key`; Python is **not** publicly exposed (private network or firewall) |
| ESP32 → Broker | TLS (port 8883) + username/password; later, per-device credentials/ACLs on topic `sentrix/telemetry/{mac}` |
| Go → Broker | Its own credential with subscribe permission |
| Device claim | 6-digit PIN checked server-side; rate-limit attempts to prevent brute force |
| Supabase RLS | Needed for Realtime so clients only see their own rows |

**Go implementation notes** (`sentrix-api`):
- Postgres via Supabase's transaction pooler (PgBouncer) does not support prepared statements; the Go pool uses pgx's simple protocol.
- The MQTT client uses a persistent session (`CleanSession=false`) with a fixed client ID, so QoS 1 messages sent while Go is down are delivered on reconnect. Run only one instance per client ID.
- Single telemetry messages must have a payload MAC matching the topic MAC. Batched messages (from the mesh root) are exempt. Readings with an invalid MAC or missing `ts` are dropped.

Never commit secrets. Use `.env` files and keep them in `.gitignore`.

---

## 10. Known Gotchas & Required Fixes

These issues exist in the current docs/schema/sample code. Fix them early.

1. **Who creates alerts?** The docs say Python (step 17), but with pre-computed bounds the check is just `value > upper OR value < lower`. **Decision: Go does it on every insert** (section 8.3), and Python stays training-only.
2. **Supabase client row limit.** The sample script uses `supabase_client.table("telemetry").select(...)`, which returns a limited number of rows by default (1000 on standard setups). 7 days at 1/min is about 10,080 rows, so you'd silently train on partial data. Query Postgres directly and aggregate hourly in SQL (section 8.5).
3. **Bounds run out.** The docs forecast only 24 hours, so after Day 9 there are no bounds. Forecast 48 hours and add a **daily job** that re-runs the forecast for every `ACTIVE` device. If no bound exists for an hour, Go skips the alert check.
4. **`is_running` doesn't exist in the schema.** The sample script filters `.eq("is_running", True)` but `Telemetry` has no such column. Either add it, or apply state gating (e.g. `rms_current >= 0.1`) in the SQL query.
5. **`prediction_bounds` needs a unique constraint** on `(device_id, timestamp)`; otherwise `upsert` just inserts duplicates.
6. **Defaults live in the database.** The migration sets `gen_random_uuid()`/`now()` column defaults and a `set_updated_at` trigger on every table with `updated_at`, so raw SQL from Go does not need to set `id`, `created_at` or `updated_at`.
7. **Hourly bounds vs 1-minute data.** ARIMA trains on hourly averages, so each bound applies to 60 one-minute readings. Expect noise. Require **3 consecutive breaches** (or a short cooldown/dedupe window) before creating an alert.
8. **Naming conflicts.** The notes use both `sentrix/data_listrik` and `sentrix/telemetry/+`, and both `/mulai-training` and `/api/calibrate`. Use the names in section 5.
9. **Web Bluetooth on Safari.** The notes contradict each other. To my knowledge, Safari on iOS does **not** support `navigator.bluetooth`, and Chrome on iOS doesn't either (it uses WebKit). Verify this yourself. For the demo, use **Chrome on Android** or **Chrome/Edge on desktop**.
10. **Python failure after status flip.** If Go flips `CALIBRATING → ACTIVE` and the Python call then fails, the device would be ACTIVE with no bounds. Section 8.2 reverts the status on failure so the next message retries.
11. **Mesh MACs.** The root publishes for leaf nodes; make sure the payload carries the originating MAC (section 5.3).
12. **Calibration clock.** `calibration_ends_at` is set when the device is claimed, but data may start flowing later. For the MVP this is fine. Later, consider setting `calibration_started_at` on the first telemetry message.
13. **`OFFLINE` has no way back yet.** Nothing marks a device `OFFLINE` or returns it to `ACTIVE`. Decide how (handshake + a `last_seen` column) before building it; naively reactivating on telemetry would skip calibration for a device that went offline while `CALIBRATING`.

---

## 11. Environment Variables

**Go**
```
DATABASE_URL=postgresql://...            # Supabase connection string (use pooler)
MQTT_BROKER_URL=tls://xxxx.hivemq.cloud:8883
MQTT_USERNAME=
MQTT_PASSWORD=
PYTHON_SERVICE_URL=http://localhost:8000
INTERNAL_API_KEY=
SUPABASE_JWT_SECRET=
VAPID_PUBLIC_KEY=
VAPID_PRIVATE_KEY=
CALIBRATION_MINUTES=10080                # 7 days; set to 5 for testing
```

**Python**
```
DATABASE_URL=postgresql://...
INTERNAL_API_KEY=                        # same value as Go
```

**Next.js**
```
NEXT_PUBLIC_SUPABASE_URL=
NEXT_PUBLIC_SUPABASE_ANON_KEY=
NEXT_PUBLIC_GO_API_URL=
NEXT_PUBLIC_VAPID_PUBLIC_KEY=
```

---

## 12. Build Order

Build in this order so every step is testable on its own.

1. **Database:** apply the migrations in `sentrix-web/supabase/migrations` (include the schema changes in section 7).
2. **Broker:** create a HiveMQ Cloud cluster and credentials. **Send the section 5.1 table to the IoT team now.**
3. **Go MQTT subscriber:** insert into `telemetry`. Test with a fake ESP32 (section 13).
4. **Python FastAPI `/api/calibrate`:** test with fake data.
5. **Go Day 8 check:** make the calibration period configurable (`CALIBRATION_MINUTES=5`) so you don't wait a week.
6. **Go REST endpoints**, then the **Next.js pages** (login → dashboard → group → sensor detail).
7. **Bounds check and alerts** in Go, then the daily forecast refresh job.
8. **Web Push** and the alerts page with RL feedback, last.
9. **Real hardware integration** with the IoT team, using the MQTT contract from section 5.

---

## 13. Testing Without Hardware

Use **MQTTX** (GUI) or `mosquitto_pub` as a fake ESP32.

```bash
# Fake handshake
mosquitto_pub -h xxxx.hivemq.cloud -p 8883 -u USER -P PASS --capath /etc/ssl/certs \
  -t sentrix/handshake \
  -m '{"mac":"AABBCCDDEEFF","pin":"123456"}'

# Fake telemetry
mosquitto_pub -h xxxx.hivemq.cloud -p 8883 -u USER -P PASS --capath /etc/ssl/certs \
  -t sentrix/telemetry/AABBCCDDEEFF \
  -m '{"mac":"AABBCCDDEEFF","rms_current":5.2,"ts":1760421000}'
```

Tips:
- Seed a `devices` row manually (status `CALIBRATING`, known MAC/PIN) before testing.
- Write a small script that publishes a sine-wave current (daily pattern) for several simulated days. This gives Python realistic data to fit.
- To test alerts, publish a spike (e.g. 15A when bounds are 4–7A).
- Test the Python trigger with `curl`:
```bash
  curl -X POST http://localhost:8000/api/calibrate \
    -H "Content-Type: application/json" -H "X-Internal-Key: dev-secret" \
    -d '{"device_id":"<uuid>"}'
```

---

## 14. Demo Checklist

- [ ] SCT-013 has its cable, and it is clamped around **one wire** (the MCB live or neutral, never both together).
- [ ] The SCT-013 cable is supported or hung securely so it doesn't pull on the clamp or board.
- [ ] Demo browser is Chrome/Edge (Web Bluetooth) over HTTPS.
- [ ] Broker credentials and the Go service are reachable from the venue Wi-Fi.
- [ ] A **pre-trained** device is ready (7-day wait is impossible live): have bounds pre-populated for the demo device, or use the shortened `CALIBRATION_MINUTES`.
- [ ] A trigger to create a visible anomaly (switch a heavy load on/off).
- [ ] Fallback: a recorded screen capture of the full flow.

---

## 15. Appendix

### 15.1 UI routes

| Route | Purpose | Key components |
|---|---|---|
| `/login`, `/signup` | Auth | Auth card, optional Google/Microsoft OAuth |
| `/dashboard` | Global overview | Metric cards (active nodes, status, anomalies), group grid, "Create Group" CTA |
| `/dashboard/[groupName]` | Group detail | Device table, BLE onboarding modal (5 steps), breadcrumbs |
| `/dashboard/[groupName]/[sensorId]` | Telemetry deep-dive | Live metric card, Recharts chart (line + shaded bounds + red markers), 1H/24H/7D toggles, local alert feed |
| `/alerts` | Global alert feed | Filters, alert cards with True/False RL buttons, resolve action |
| `/settings` | Admin | Profile, org invites, notification preferences |

BLE onboarding modal steps: scanning → pick MAC → 6-digit PIN → confirm Wi-Fi → success.

### 15.2 Hardware summary

- SCT-013 split-core CT (up to 100A AC) → 1.65V DC bias divider → ESP32 ADC → RMS calculation.
- HLK-PM01 AC-DC module powers the board from line voltage.
- Calibration phase flow: state gating (standby vs active) → load classification (first 60 s) → Auto-ARIMA calibration (7 days) → anomaly scoring.

### 15.3 Device lifecycle

```
UNCLAIMED ──(user claims via Next.js → Go)──► CALIBRATING ──(7 days, Go triggers Python)──► ACTIVE
                                                                                              │
                                                              (connection lost) ◄─────────────┘
                                                                  OFFLINE
```

### 15.4 Who-does-what summary

1. **User's phone/browser → ESP32 (Bluetooth):** once, to hand over the building's Wi-Fi credentials.
2. **ESP32 → MQTT broker → Go → Supabase:** every 1 minute, to send and store the current reading.
3. **Go → Python (HTTP POST):** once on Day 8, when `now() > calibration_ends_at`.
4. **Python → Supabase:** fits Auto-ARIMA on the last 7 days and stores `upper_bound` / `lower_bound` in `prediction_bounds`.
5. **Supabase → Next.js (REST + Realtime):** the website shows the charts from `telemetry` + `prediction_bounds` and pops up notifications when a new row appears in `alerts`.

### 15.5 4-week sprint (summary)

| Week | IoT | Web | ML |
|---|---|---|---|
| 1 | Sensor harness, base MQTT firmware, 1kHz sampling | Backend/DB setup, auth, API routing | Data cleaning, EDA, feature pipeline |
| 2 | TLS, ring buffer, JSON payload schema | WebSocket ingestion, dashboard + chart | Baseline model, evaluation |
| 3 | Field install, calibration, OTA verify | Alert engine, asset detail views | Model deployment/optimization |
| 4 | Stress and disconnect-recovery testing, docs | Roles, exports, UI polish, docs | Validation, threshold tuning, docs |

> The sprint doc mentions Isolation Forest / ONNX / TFLite edge inference, while the architecture docs use Auto-ARIMA on the server. Align on one approach for the MVP (this guide assumes Auto-ARIMA).