# Clinic Booking API — Plan & Pseudocode

Spec: https://koyaacademy.com/assessment-api/openapi.json (OpenAPI 3.0.3, mirrored in `docs/openapi.json`).

## Stack

| Concern  | Choice | Why |
|----------|--------|-----|
| API      | Go 1.22+ `net/http` (method + path routing in stdlib) | No framework needed for 6 routes |
| DB       | PostgreSQL (Neon free tier) via `pgx/v5` | Durable; unique indexes + advisory locks give the concurrency guarantees |
| Host     | Render (free web service, Docker) | HTTPS out of the box, long-running process |
| Web page | Static HTML/JS embedded with `embed.FS`, served by the same Go binary at `/` | Same origin → no CORS, one deploy |
| Schema   | One embedded `schema.sql` with `IF NOT EXISTS`, run on startup | No migration tool for 3 tables |
| Rate limit | In-memory mutex map | One instance on Render; shortcut noted in code |
| API docs | Swagger UI (jsdelivr CDN) at `/docs`, loading the official spec served at `/openapi.json` | Docs are the graded contract itself, can't drift; Try-it-out works live for the demo |
| Layout   | One `package main`: `main.go`, `handlers.go`, `store.go`, `schema.sql`, `web/index.html` | Ponytail: no layers until needed |

## Contract summary (what the checker will hit)

| Method | Path | Level | Success | Errors |
|--------|------|-------|---------|--------|
| POST | `/reset` | 1 | 200 | 400 bad body |
| GET | `/slots?provider=` | 1 | 200 `{slots:[...available]}` sorted by `start` asc | — |
| POST | `/bookings` | 1–5 | 201 Booking | 400, 404, 409, 422, 429 (+`Retry-After`) |
| GET | `/bookings/{id}` | 1 | 200 Booking | 404 |
| DELETE | `/bookings/{id}` | 3 | 200 cancelled Booking | 404, 409 already cancelled |
| GET | `/bookings?email=&limit=&cursor=` | 4 | 200 `{bookings, next_cursor}` oldest first | 400 bad limit |

All requests: `X-API-Key` required → 401. Every error body: `{"error": "<message>"}`.

## Schema

```sql
CREATE TABLE slots (
  id        TEXT PRIMARY KEY,
  provider  TEXT NOT NULL,
  start_at  TIMESTAMPTZ NOT NULL,
  end_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX slots_provider_start ON slots (provider, start_at);

CREATE TABLE bookings (
  seq        BIGSERIAL UNIQUE,            -- stable ordering for cursor pagination
  id         TEXT PRIMARY KEY,            -- random, e.g. "bk_" + 16 hex chars
  slot_id    TEXT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  email      TEXT NOT NULL,               -- stored trimmed + lower-cased
  status     TEXT NOT NULL CHECK (status IN ('confirmed','cancelled')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- THE double-booking guarantee (Level 3): at most one confirmed booking per slot.
CREATE UNIQUE INDEX one_confirmed_per_slot ON bookings (slot_id) WHERE status = 'confirmed';
CREATE INDEX bookings_email_seq ON bookings (email, seq);

CREATE TABLE idempotency_keys (           -- Level 4
  key          TEXT PRIMARY KEY,
  request_hash TEXT NOT NULL,             -- sha256 of normalised body
  status_code  INT  NOT NULL,
  response     JSONB NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

## Pseudocode

### Middleware chain (outermost first)

```
catch-all "/" route → serves the page at exactly "/", otherwise 404 {"error":"not found"}
requireAPIKey:
    if path in {"/", "/docs", "/openapi.json"}: skip   # page + docs are public; Swagger's Authorize sends the key
    if method == OPTIONS: skip
    key := header["X-API-Key"]
    if key == "" or !constantTimeEqual(key, env.API_KEY): 401 {"error":"invalid or missing api key"}
limitBodySize(16 KB)
```

### POST /reset (L1)

```
body := decode {slots: [Slot]} ; on error → 400
for each slot: require id, provider, start, end (RFC3339) and end > start ; else 400
TX:
    DELETE FROM bookings; DELETE FROM idempotency_keys ; limiter.Reset()
    DELETE FROM slots
    bulk INSERT slots                       # past slots allowed
200 {"status":"ok"}
```

### GET /slots (L1)

```
provider := query["provider"]               # optional, exact match
rows := SELECT s.*, NOT EXISTS(confirmed booking for s.id) AS available
        FROM slots s [WHERE provider = $1] ORDER BY start_at, id
200 {"slots": rows}                         # always an array, never null
```

### POST /bookings (L1–L5) — the core

```
raw := read body
in  := decode NewBooking ; malformed JSON → treat fields as missing (still counted below if email recoverable)
email := lower(trim(in.email)) ; name := trim(in.name) ; slotID := trim(in.slot_id)

# L4 — idempotent replay happens BEFORE rate limiting: a retry is the same attempt, not a new one
idemKey := header["Idempotency-Key"]
if idemKey != "":
    TX begins; pg_advisory_xact_lock(hash(idemKey))       # serialises concurrent retries of the same key
    hash := sha256(canonical(slotID, name, email))
    if row := SELECT FROM idempotency_keys WHERE key = idemKey:
        if row.request_hash != hash: 422 {"error":"idempotency key reused with a different body"}
        else: replay row.status_code + row.response       # same status, same booking id
        return

# L5 — rate limit: 5 attempts / email / rolling 60s, failures included
if email != "":
    limiter.mu.Lock()
    times := attempts[email] filtered to the last 60s
    if len(times) >= 5:
        retry := ceil(times[0] + 60s - now) (min 1)
        429 + header Retry-After: retry        # NOT stored under idempotency key
        return
    attempts[email] = append(times, now) ; unlock

# L2 — validation, in this order
if slotID == "" or name == "" or email == "": resp = 400 "slot_id, name and email are required"
else if !validEmail(email):                   resp = 400 "invalid email"
else if slot := SELECT slot WHERE id = slotID ; none: resp = 404 "slot not found"
else if slot.start_at <= now():               resp = 400 "slot has already started"
else:
    # L3 — the partial unique index is the lock; no read-then-write race possible
    INSERT INTO bookings(id, slot_id, name, email, 'confirmed')
    on unique_violation(one_confirmed_per_slot): resp = 409 "slot already booked"
    else:                                        resp = 201 booking

if idemKey != "": INSERT idempotency_keys(idemKey, hash, resp.status, resp.body)
COMMIT ; write resp
```

`validEmail`: `net/mail.ParseAddress(e)` succeeds, `addr.Address == e` (rejects `"Ada <a@b.com>"`), domain contains a dot, length ≤ 254.

### GET /bookings/{id} (L1)

```
row := SELECT booking WHERE id = $1 ; none → 404 "booking not found"
200 row
```

### DELETE /bookings/{id} (L3)

```
UPDATE bookings SET status='cancelled' WHERE id=$1 AND status='confirmed' RETURNING *
if updated: 200 row                         # partial index no longer covers it → slot is free
else if exists(id): 409 "booking already cancelled"
else: 404 "booking not found"
```

### GET /bookings?email= (L4)

```
email := lower(trim(q.email)) ; empty → 400 "email is required"
limit := q.limit default 10 ; not an int or outside 1..50 → 400 "limit must be between 1 and 50"
afterSeq := 0 ; if q.cursor != "": afterSeq = decodeCursor(q.cursor) ; bad → 400 "invalid cursor"
rows := SELECT ... WHERE email = $1 AND seq > afterSeq ORDER BY seq LIMIT limit + 1
next := rows has limit+1 ? encodeCursor(rows[limit-1].seq) : null
200 {"bookings": rows[:limit], "next_cursor": next}
```

Cursor = base64url of the last `seq` (opaque to clients; keyset pagination stays correct while new bookings arrive).

### Web page (`/`)

```
on load: ask for API key once (kept in sessionStorage), GET /slots → render cards grouped by provider
provider filter dropdown → GET /slots?provider=
click available slot → form (name, email) → POST /bookings with a fresh Idempotency-Key (crypto.randomUUID)
    201 → confirmation panel (booking id, provider, time, status) + refresh slots
    4xx → show error message from {"error"}
"Find my bookings" → GET /bookings?email= with "Load more" using next_cursor
cancel button on a confirmed booking → DELETE /bookings/{id}
```

## Delivery order (one PR per level)

1. **Scaffold** — module, env config, Postgres connect + `schema.sql`, JSON helpers, Dockerfile
2. **Level 1** — reset, slots, create booking, get booking
3. **Level 2** — API key middleware, validation, 404/400 rules
4. **Level 3** — partial unique index + 409, concurrency test (50 goroutines, exactly one 201), cancel
5. **Level 4** — idempotency keys, cursor pagination
6. **Level 5** — per-email rate limit + `Retry-After`
7. **Web page** — embedded static UI
8. **Deploy** — Neon + Render, smoke test every endpoint against the live URL

## Decisions (confirmed 2026-11-12)

- **API key on the web page:** the page asks for the key once and keeps it in `sessionStorage`; it is never committed or embedded. The per-email rate limit is the abuse guard.
- **Idempotent replays don't count toward the rate limit:** the replay check runs first, because a retry is the same attempt.
- **`/reset` clears rate-limit history** so the grader's test runs start clean.
- **Swagger:** serve the official spec, don't generate one. The only edit to our copy is a `servers: [{url: "/"}]` entry.
