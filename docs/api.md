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

Planned (see `sentrix-architecture.md` section 5.5): groups, device claim, telemetry and bounds reads, alerts, push subscriptions.

## Adding a route

Register it in `internal/httpapi/server.go` `Handler()`. Wrap it in `s.auth.RequireAuth(...)` if it needs a user, and read the user with `httpapi.UserID(r.Context())`. Every group, device and alert route must check that the resource belongs to that user.
