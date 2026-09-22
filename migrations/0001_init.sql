-- 0001_init: event store, projection and idempotency keys.
CREATE TABLE IF NOT EXISTS request_events (
    id          TEXT        PRIMARY KEY,
    request_id  TEXT        NOT NULL,
    seq         INTEGER     NOT NULL,
    type        TEXT        NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    actor       TEXT,
    trace_id    TEXT,
    payload     JSONB       NOT NULL,
    UNIQUE (request_id, seq)
);
CREATE INDEX IF NOT EXISTS request_events_request_idx ON request_events (request_id, seq);
CREATE INDEX IF NOT EXISTS request_events_at_idx ON request_events (at);

CREATE TABLE IF NOT EXISTS request_views (
    id          TEXT        PRIMARY KEY,
    state       TEXT        NOT NULL,
    payer       TEXT        NOT NULL,
    version     INTEGER     NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    doc         JSONB       NOT NULL
);
CREATE INDEX IF NOT EXISTS request_views_state_idx ON request_views (state, updated_at DESC);
CREATE INDEX IF NOT EXISTS request_views_payer_idx ON request_views (payer);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key         TEXT        PRIMARY KEY,
    request_id  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
