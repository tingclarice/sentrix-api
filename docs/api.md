# sentrix-api: REST API

Base URL in development: `http://localhost:8080` (`PORT`). Browser origins listed in `ALLOWED_ORIGINS` (comma-separated, default `http://localhost:3000`) may call it.

All errors are JSON: `{"error": "message"}`.

## Authentication

Every route except `/health` needs the Supabase access token of the logged-in user:

```
Authorization: Bearer <supabase access_token>
```

In Next.js: `const { data } = await supabase.auth.getSession(); data.session?.access_token`.

The server verifies the token locally (signature, expiry, audience `authenticated`). Projects with the current asymmetric keys (ES256/RS256) need `SUPABASE_URL` set; legacy projects with a shared secret (HS256) need `SUPABASE_JWT_SECRET`. A missing/invalid token gives `401`. If neither is set, protected routes give `503`.

## Routes

| Method | Path | Auth | Response |
|---|---|---|---|
| GET | `/health` | no | `200 {"status":"ok"}`, or `503` if the database is unreachable |
| GET | `/api/me` | yes | `200 {"user_id":"<uuid>"}`: confirms a token works |
| POST | `/api/groups` | yes | `201` group |
| GET | `/api/groups` | yes | `200` array of the user's groups (`[]` if none) |
| POST | `/api/groups/{id}/devices` | yes | `200` device (claim) |
| GET | `/api/groups/{id}` | yes | `200` group |
| GET | `/api/groups/{id}/devices` | yes | `200` array of the group's devices |
| GET | `/api/devices/{id}/telemetry?range=1h\|24h\|7d` | yes | `200` chart points (default `24h`) |
| GET | `/api/devices/{id}/bounds?range=1h\|24h\|7d` | yes | `200` predicted safe zone (default `24h`) |
| GET | `/api/dashboard/summary` | yes | `200` overview counts |

Planned (see `sentrix-architecture.md` section 5.5): alerts (list, feedback, resolve) and push subscriptions.

Path IDs must be UUIDs. A malformed ID, a missing resource and someone else's resource all return the same `404`.

### Objects

```jsonc
// group
{ "id": "uuid", "name": "Building A", "description": "north wing" /* or null */,
  "mesh_id": "0123456789AB", "created_at": "2026-10-20T08:00:00Z" }

// device. The PIN is never returned.
{ "id": "uuid", "mac_address": "AABBCCDDEEFF", "name": "Pump 1" /* or null */,
  "group_id": "uuid" /* or null */, "status": "CALIBRATING",
  "calibration_started_at": "...", "calibration_ends_at": "...", "created_at": "..." }
```

`status` is one of `UNCLAIMED`, `CALIBRATING`, `ACTIVE`, `OFFLINE`.

### POST /api/groups

Body: `{"name": "Building A", "description": "optional"}`. `name` is required (1 to 100 characters after trimming), `description` at most 500. Go generates the unique 6-byte `mesh_id` (12 uppercase hex characters); the frontend sends it to the ESP32 over Bluetooth during onboarding.

### POST /api/groups/{id}/devices (claim a device)

Body: `{"mac": "AABBCCDDEEFF", "pin": "123456", "name": "optional"}`.

- `mac`: 12 hex characters, no colons (lowercase is accepted and uppercased).
- `pin`: the 6 digits printed with the MAC on the device.

On success the device moves from `UNCLAIMED` to `CALIBRATING`, is attached to the group, and `calibration_ends_at` is set `CALIBRATION_MINUTES` ahead (default 10080 = 7 days; set it to 5 to test quickly).

| Status | Meaning |
|---|---|
| 400 | malformed `mac`, `pin`, `name` or JSON |
| 404 | the group is not yours / does not exist, **or** no unclaimed device matches that MAC and PIN. Wrong PIN, unknown MAC and already-claimed look identical on purpose. |
| 429 | too many failed attempts; see the `Retry-After` header (seconds) |

Failed claims are limited to 5 per user and 20 per MAC every 15 minutes. The counters are in memory and reset when the server restarts.

### GET /api/groups/{id}/devices

`404` if the group is not yours. Otherwise the array of devices in it.

### GET /api/devices/{id}/telemetry

```jsonc
{ "range": "24h", "bucket_seconds": 300,
  "points": [ { "timestamp": "2026-10-20T08:00:00Z", "rms_current": 4.1, "max_rms_current": 9.8 } ] }
```

`range=1h` returns raw one-minute readings (`bucket_seconds` is 0 and `max_rms_current` equals `rms_current`). `24h` averages into 5-minute buckets and `7d` into 15-minute buckets, so a chart gets at most about 700 points. In a bucketed point `rms_current` is the average and `max_rms_current` is the peak, so a short spike stays visible; draw the line from `rms_current` and mark peaks from `max_rms_current`. Points are in time order; gaps are not filled. A bad `range` is `400`.

### GET /api/devices/{id}/bounds

```jsonc
{ "range": "24h",
  "bounds": [ { "timestamp": "2026-10-20T08:00:00Z", "upper_bound": 7.2, "lower_bound": 3.1 } ] }
```

One entry per hour, from the start of the window (rounded down to the hour) to the last forecast hour, so it includes hours still in the future. Each bound applies to the whole hour starting at its `timestamp`. The array is empty until the device has finished calibration and the ML service has written bounds.

### GET /api/dashboard/summary

```json
{ "groups": 2, "devices": 3, "calibrating": 1, "active": 2, "offline": 0,
  "anomalies_24h": 0, "unresolved_alerts": 0 }
```

Counts across all of the user's groups. `anomalies_24h` and `unresolved_alerts` come from the `alerts` table, which stays at 0 until the alert engine exists.

## Adding a route

Register it in `internal/httpapi/server.go` `Handler()`. Wrap it in `s.auth.RequireAuth(...)` if it needs a user, and read the user with `httpapi.UserID(r.Context())`. Every group, device and alert route must check that the resource belongs to that user.
