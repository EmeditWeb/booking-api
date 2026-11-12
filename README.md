# Clinic Booking API

A clinic appointment booking API written in Go and backed by PostgreSQL, with a small web page that uses it. Patients can see open slots, book one, get a confirmation, look up and page through their bookings, and cancel.

It's built to the [Koya Academy assessment spec](https://koyaacademy.com/assessment-api). The same spec ships in this repo as [`docs/openapi.json`](docs/openapi.json) and is served live as interactive Swagger docs at `/docs`.

| | |
|---|---|
| **Live API** | _added at deploy_ |
| **Web page** | _added at deploy_ (`/`) |
| **Swagger docs** | _added at deploy_ (`/docs`) |
| **Stack** | Go 1.22 (`net/http`), PostgreSQL (Neon), `pgx/v5`, Docker, Render |

---

## Progress

| Level | Requirement | Status |
|-------|-------------|--------|
| 0 | Scaffold: config, database, schema, Swagger docs, Dockerfile | ✅ |
| 1 | List slots, book one, look up a booking | ✅ |
| 2 | API key on every request; reject missing or invalid booking details | ✅ |
| 3 | No double booking, even for simultaneous requests; cancellation | ✅ |
| 4 | Idempotent retries (`Idempotency-Key`); cursor pagination of a person's bookings | ✅ |
| 5 | Rate limit: 5 booking attempts per email per minute, plus a per-IP flood guard on every route | ✅ |
| — | Web page | ⏳ |
| — | Deployed over HTTPS | ⏳ |

---

## API at a glance

Every request needs the `X-API-Key` header (401 otherwise). Everything is JSON, and every error looks like `{"error": "<message>"}`.

| Method | Path | What it does | Errors |
|--------|------|--------------|--------|
| `POST` | `/reset` | Replace all slots and delete every booking | 400 |
| `GET` | `/slots?provider=` | All slots, earliest first, each with `available` | — |
| `POST` | `/bookings` | Book a slot (optional `Idempotency-Key` header) | 400, 404, 409, 422, 429 |
| `GET` | `/bookings/{id}` | Look up a booking | 404 |
| `DELETE` | `/bookings/{id}` | Cancel a booking and free its slot | 404, 409 |
| `GET` | `/bookings?email=&limit=&cursor=` | A person's bookings, oldest first, paginated | 400 |

Public, no key needed: `/` (the web page), `/docs` (Swagger UI), `/openapi.json`.

### Example

```bash
API=https://<your-deployment>
KEY=<your api key>

# Load slots
curl -X POST "$API/reset" -H "X-API-Key: $KEY" -H "Content-Type: application/json" -d '{
  "slots": [{"id":"s1","provider":"Dr Ade","start":"2026-12-10T09:00:00Z","end":"2026-12-10T09:30:00Z"}]
}'

# Book it (safe to retry: same key gives the same booking)
curl -X POST "$API/bookings" -H "X-API-Key: $KEY" -H "Idempotency-Key: 7f1c" \
  -H "Content-Type: application/json" -d '{"slot_id":"s1","name":"Ada Obi","email":"ada@example.com"}'
# 201 {"id":"bk_…","slot_id":"s1","name":"Ada Obi","email":"ada@example.com","status":"confirmed","created_at":"…"}
```

---

## How it works

### No double booking (level 3)

The **database** enforces this, not application code:

```sql
CREATE UNIQUE INDEX one_confirmed_per_slot ON bookings (slot_id) WHERE status = 'confirmed';
```

A "check if free, then insert" in Go has a race: two requests can both see the slot as free. Instead, every booking is a plain `INSERT`, and Postgres lets only one confirmed row per slot through. The loser gets a unique violation, which becomes **409**. Cancelling flips the row to `cancelled`, the index stops covering it, and the slot is free again. A test fires 50 simultaneous bookings at one slot and asserts exactly one 201.

### Safe retries (level 4)

The client sends an `Idempotency-Key`, and the server stores that key with a SHA-256 of the body's field values (so whitespace and key order don't matter) and the exact bytes of the response it sent. A retry with the same key and body replays the stored response: same status, same booking id, no new row. The same key with a different body gets **422**. A Postgres advisory lock on the key, held for the whole transaction, makes two simultaneous retries line up instead of both booking. The booking and the stored response commit in the same transaction, so a crash can never leave one without the other.

### Pagination (level 4)

Pagination is keyset (cursor) based, not `OFFSET`. Each booking has an increasing `seq`, and `next_cursor` is an opaque, encoded "last seq seen". Pages stay correct while new bookings arrive, and every page costs one index range scan.

### Rate limiting (level 5)

Each email gets at most 5 booking attempts in any rolling 60 seconds, failed attempts included. The 6th returns **429** with `Retry-After` set to the whole seconds until the oldest attempt leaves the window. Emails are trimmed and lower-cased first, so `ADA@x.com ` and `ada@x.com` share one budget. Idempotent replays don't count, because a retry is the same attempt, and `/reset` clears the history.

Separately, **every route** (including the public page and docs) has a per-IP flood guard of 1,000 requests per 10 seconds. It sits in front of the API-key check, so it also slows down key guessing. It's set high on purpose: a real client or a test suite firing 50 parallel bookings never comes close, but a script hammering the server does. The client IP comes from the *last* `X-Forwarded-For` hop, the one our hosting proxy adds, because the first hop is whatever the client claims.

Both limiters are in-memory sliding windows, which is right for a single instance. With more instances they'd move to Postgres or Redis.

### Order of checks in `POST /bookings`

```
IP flood guard → API key → idempotency replay → per-email rate limit → required fields / email format (400)
       → slot exists (404) → slot not started (400) → insert (201 / 409) → store idempotent response
```

---

## Running locally

Prerequisites: Go 1.22+, Docker.

```bash
# 1. Postgres
docker run -d --name booking-pg -e POSTGRES_PASSWORD=pg -p 5432:5432 postgres:16-alpine

# 2. Config
cp .env.example .env    # then set API_KEY to any secret string
export $(grep -v '^#' .env | xargs)

# 3. Run (the schema is applied automatically on startup)
go run .
```

Then open http://localhost:8080 for the page, or http://localhost:8080/docs for Swagger.

### Tests

The tests run against the real HTTP handlers and a real Postgres, so the concurrency and idempotency guarantees are actually exercised:

```bash
TEST_DATABASE_URL=postgres://postgres:pg@localhost:5432/postgres?sslmode=disable go test -race ./...
```

### Environment

| Variable | Required | Description |
|----------|----------|-------------|
| `DATABASE_URL` | yes | Postgres connection string |
| `API_KEY` | yes | The key clients send in `X-API-Key`. The server won't start without it. |
| `PORT` | no | Defaults to `8080` |

---

## Project layout

Deliberately flat. It's one small service, so there are no layers until something needs them.

```
main.go          config, startup, routing, middleware
handlers.go      HTTP handlers and JSON helpers
store.go         all SQL
ratelimit.go     sliding-window limiter, per-IP middleware
schema.sql       tables and indexes (embedded, idempotent)
web/index.html   the booking page (embedded)
docs/            official OpenAPI spec and implementation plan
api_test.go      end-to-end tests against Postgres
ratelimit_test.go limiter unit tests with a fake clock
Dockerfile       two-stage build into a distroless image (~19 MB)
```

Design notes and pseudocode: [`docs/PLAN.md`](docs/PLAN.md).
