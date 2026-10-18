# sentrix-api: Handoff Notes

Summary of the planning chat so a new session can continue from here. Written 2026-10-18.
Full design reference: `sentrix-architecture.md` in this repo's `docs/` (a copy also lives in `sentrix-web/docs/`; keep this one as the source and sync the other, or delete the copy). Read it before changing contracts.

## Repo layout (as of today)

| Repo | Role | State |
|---|---|---|
| `tingclarice/sentrix-web` | Next.js frontend. **Renamed from `sentrix`**, so it still holds the old monorepo layout (`frontend/`, empty `backend/` `ml/` `firmware/`), plus `supabase/` and `docs/` | Has code |
| `tingclarice/sentrix-api` | This repo. Go backend | Scaffold, nothing committed or pushed yet (GitHub repo is empty) |
| `tingclarice/sentrix-ml` | Python FastAPI + Auto-ARIMA | Empty |
| `tingclarice/sentrix-firmware` | ESP32 (IoT team) | Empty |

Local folders: `/Users/abbie/Documents/sem 5/sentrix/{sentrix, sentrix-api, sentrix-ml, sentrix-firmware}` (`sentrix` is the local `sentrix-web` checkout).

Decisions so far:
- The DB schema and architecture docs **stay in `sentrix-web`** (`supabase/`, `docs/`). Not moved. Add a README note here pointing to it.
- Optional later cleanup in `sentrix-web`: delete empty `backend/ ml/ firmware/` placeholders. Also a stray remote branch `new-frontend` was not inspected.

## How the system fits together (short)

ESP32 -> MQTT broker -> **Go (this repo)** -> Supabase Postgres <- Python (training only) ; Next.js reads via Go REST + Supabase Realtime.
- Go subscribes to `sentrix/handshake` and `sentrix/telemetry/+` (QoS 1). MAC format: uppercase 12 hex, no colons.
- Go inserts telemetry, runs the Day 8 check (atomic `CALIBRATING -> ACTIVE`, then `POST /api/calibrate` to Python with `X-Internal-Key`; revert status if Python fails), does bounds checks and alerts, serves REST, sends Web Push.
- Python never talks to the browser; Next.js never calls Python.

## What exists in this repo now (uncommitted)

- `go.mod` (module `github.com/tingclarice/sentrix-api`), `go.sum`, `.gitignore` (ignores `.env`), `.env.example`
- `cmd/api/main.go`: loads config, connects Postgres (pgx pool, simple protocol for PgBouncer, pinged at startup) + MQTT, graceful shutdown on SIGINT/SIGTERM
- `internal/config/config.go`: env vars (`DATABASE_URL`, `MQTT_*`, `PYTHON_SERVICE_URL`, `INTERNAL_API_KEY`)
- `internal/ingest/ingest.go`: MQTT subscribe, telemetry insert (accepts single or batched payload; validates MAC/`ts`, single readings must match the topic MAC; persistent MQTT session), skips `UNCLAIMED`, Day 8 atomic flip + Python trigger with revert
- Builds and vets clean. **Never run against a real broker/DB yet.**

## Known gaps / fixes to make

1. `ON CONFLICT DO NOTHING` on the telemetry insert is a no-op: schema has no unique key on `telemetry(device_id, timestamp)`. Add the migration from architecture section 7 (in `sentrix-web/supabase/migrations`), or drop the clause.
2. `handleHandshake` is a stub. Needs to verify MAC + PIN (Flow 1 step 8) and mark the device online. Schema has no "online" field yet, so decide how (status, or a `last_seen` column).
3. `CALIBRATION_MINUTES` is not read. It belongs in the device-claim endpoint (`calibration_ends_at = now() + CALIBRATION_MINUTES`).
4. No bounds check / alerts yet (arch 8.3). Require **3 consecutive breaches** or a cooldown before inserting an alert (gotcha #7). Skip if no bound exists for that hour.
5. No REST API yet (arch 5.5), no Supabase JWT check, no ownership check, no PIN rate-limit.
6. No Web Push yet (needs VAPID keys + `push_subscriptions` table, which exists).
7. Config still needs `SUPABASE_JWT_SECRET`, `VAPID_*`, `CALIBRATION_MINUTES` (arch section 11).
8. Schema defaults are DB-level (migration header says ids/timestamps/`updated_at` handled by DB triggers), so raw SQL inserts from Go are fine. Verify when adding the first alert insert.
9. `OFFLINE` devices never return to `ACTIVE` and nothing sets `OFFLINE` (see architecture gotcha #13). Decide together with the handshake/`last_seen` design.
10. `Subscribe` failures are logged but not retried; fine for now.

## Open team decisions (from `sentrix-web/docs/progress.md` + arch doc)

- Telemetry payload: single vs batched (Go accepts both for now).
- BLE payload size / MTU chunking.
- ML approach: Auto-ARIMA on the server (arch doc) vs Isolation Forest / edge inference (sprint doc). Assumed Auto-ARIMA.
- Whether the frontend reads devices via Supabase directly or only via Go (`devices.pin_code` is visible to the owner on direct reads).

## Environment status

- Supabase dev project exists and the user confirmed the tables are present. Not yet verified: the second migration (signup name fix) and whether the seed device `AABBCCDDEEFF` / PIN `123456` exists. Check with `select mac_address, status from devices;`.
- Mosquitto was **not installed** at last check. `psql` and `brew` are installed.
- `sentrix-api/.env` does **not exist yet**.

## Next steps (in order)

1. `brew install mosquitto && brew services start mosquitto`
2. Create `.env` from `.env.example`: `DATABASE_URL` = Supabase **Transaction pooler** string (Project Settings -> Database; quote it in `.env` if it contains `&` or `#`), `MQTT_BROKER_URL=tcp://localhost:1883`, `INTERNAL_API_KEY=dev-secret`.
3. In the Supabase SQL Editor:
   ```sql
   update devices set status='CALIBRATING', calibration_ends_at = now() + interval '5 minutes'
   where mac_address='AABBCCDDEEFF';
   ```
4. `set -a; source .env; set +a; go run ./cmd/api` -> expect `MQTT connected and subscribed`.
5. `mosquitto_pub -t sentrix/telemetry/AABBCCDDEEFF -m '{"mac":"AABBCCDDEEFF","rms_current":5.2,"ts":'$(date +%s)'}'` then `select * from telemetry;` -> a row means ingest works.
6. Test Day 8 with `calibration_ends_at` in the past. Expect a Python failure log and a revert to `CALIBRATING` (no ML service yet), or use a stub returning 202.
7. Write a simulator (sine-wave daily pattern, can inject spikes). Reused for ML training data and alert demos.
8. Add `CALIBRATION_MINUTES`, handshake handling, then REST API (`/health`, groups, claim device, telemetry/bounds reads, then JWT + ownership), then alerts, then Web Push.
9. First commit: do it on a branch and open a PR. The GitHub repo is empty, so the first push creates `main`. Nothing has been committed or pushed yet, by the user's request.
10. Later: CI for Go (`go vet`, `go build`, `go test`), copy the frontend CI pattern from `sentrix-web/.github/workflows/frontend-ci.yml`.

## Working agreements

- Do not commit or push unless asked.
- Never paste secrets (DB password, JWT secret, VAPID private key) into chat; they live in `.env`.
