-- Applied on every startup, so every statement must be idempotent.

CREATE TABLE IF NOT EXISTS slots (
  id       TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  start_at TIMESTAMPTZ NOT NULL,
  end_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS slots_start ON slots (start_at, id);

CREATE TABLE IF NOT EXISTS bookings (
  seq        BIGSERIAL UNIQUE, -- insertion order, used as the pagination cursor
  id         TEXT PRIMARY KEY,
  slot_id    TEXT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  email      TEXT NOT NULL,    -- stored trimmed and lower-cased
  status     TEXT NOT NULL CHECK (status IN ('confirmed', 'cancelled')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The double-booking guarantee: at most one confirmed booking per slot, enforced by Postgres.
CREATE UNIQUE INDEX IF NOT EXISTS one_confirmed_per_slot ON bookings (slot_id) WHERE status = 'confirmed';
CREATE INDEX IF NOT EXISTS bookings_email_seq ON bookings (email, seq);

CREATE TABLE IF NOT EXISTS idempotency_keys (
  key          TEXT PRIMARY KEY,
  request_hash TEXT NOT NULL,
  status_code  INT NOT NULL,
  response     JSONB NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
