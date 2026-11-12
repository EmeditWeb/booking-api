# Booking API — Project Rules

## Source of truth
- The OpenAPI spec (`docs/openapi.json`, from koyaacademy.com/assessment-api) wins over anything else. Status codes, field names, JSON shapes and header names must match it exactly.
- Every error response is `{"error": "<message>"}`. No other error shape, including 404/405 on unknown routes and 500s.
- Plan and pseudocode live in `docs/PLAN.md`. Update it when a decision changes.

## Go
- Go 1.22+, stdlib `net/http` routing, `pgx/v5` for Postgres. That is the only dependency; add another only if the stdlib can't do the job in a few lines.
- Ponytail mode: the smallest change that fully works. One `package main`, a few flat files: `main.go` (config + routes + middleware), `handlers.go`, `store.go` (all SQL), `schema.sql` (embedded, idempotent `IF NOT EXISTS`), `web/index.html` (embedded). No service/repository layers, interfaces, or config frameworks until something forces them.
- Always pass `context.Context`; wrap errors with `fmt.Errorf("...: %w", err)`; never ignore an error.
- Times are stored as `TIMESTAMPTZ` and returned as RFC3339 UTC.
- `gofmt`, `go vet` and `go test -race ./...` must pass before every commit.

## Correctness guarantees (never weaken)
- Double booking is prevented by the database (partial unique index), never by a read-then-write check in Go.
- Idempotency is serialised with a Postgres advisory lock inside the booking transaction, so concurrent retries never create two bookings.
- The rate limiter is an in-memory mutex map (`shortcut: per-instance and resets on restart; move to Postgres if we ever run >1 instance`).
- Emails are trimmed and lower-cased before validation, storage, lookup, and rate limiting.

## Security
- The API key comes from the `API_KEY` env var; the server refuses to start without it. Compare it in constant time.
- No secrets in git: `.env` is gitignored; `.env.example` lists the variable names only.
- Parameterised SQL only. Request bodies are capped in size. Never log API keys or full request bodies.

## Testing
- Write the test first for each level (TDD): one table-driven `api_test.go` that drives the real handler via `httptest` against a real Postgres (`TEST_DATABASE_URL`, Docker locally).
- Level 3 needs a concurrency test: 50 parallel requests for one slot → exactly one 201, the rest 409.
- Every status code in the spec gets at least one test row. Coverage follows from that; don't chase a number with filler tests.

## Git & PRs
- One branch and one PR per level: `feat/level-1-core-booking`, `feat/level-2-auth-validation`, and so on.
- Conventional commits: `feat:`, `fix:`, `test:`, `docs:`, `chore:`, `ci:`, `refactor:`.
- **No AI attribution anywhere**: no `Co-Authored-By: Claude` lines and no "Generated with Claude Code" in commit messages, PR titles or PR descriptions.
- Every PR description has these sections:
  1. **Summary**: what level or feature it delivers and why
  2. **Changes**: a file-by-file or area-by-area list
  3. **Spec compliance**: each spec rule it covers, mapped to the code or test that proves it
  4. **Design decisions**: alternatives considered and trade-offs
  5. **How to test**: exact commands and curl examples
  6. **Test plan**: a checklist of what was verified
  7. **Follow-ups**: known gaps or the next level
- Don't merge with failing CI. Once the submission is in, freeze `main` and the deployment.
