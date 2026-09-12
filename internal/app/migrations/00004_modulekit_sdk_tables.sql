-- +goose Up
-- Idempotency markers for event consumers (ADR 0006 decision 4).
--
-- The event backbone delivers at least once, so every consumer will eventually
-- be handed the same event twice. This table is what makes the second delivery
-- a no-op instead of a duplicated side effect.
--
-- The composite PRIMARY KEY is the mechanism, not a lookup optimisation. Two
-- concurrent deliveries of one event race to insert; the loser violates the key
-- and is recognised as a duplicate. A check-then-insert would let both pass the
-- check, which is exactly the bug this exists to prevent — so this must stay a
-- real constraint and never be relaxed to a plain index.
--
-- tenant_id leads the key because the table is tenant-scoped like any other:
-- one tenant's consumer must not be able to observe, or suppress, another
-- tenant's processing.
--
-- consumer is part of the key because the same event legitimately reaches
-- several consumers, and each must process it exactly once.
CREATE TABLE IF NOT EXISTS modulekit_processed_events (
    tenant_id    TEXT NOT NULL,
    consumer     TEXT NOT NULL,
    event_id     TEXT NOT NULL,
    processed_at TIMESTAMP NOT NULL,
    PRIMARY KEY (tenant_id, consumer, event_id)
);

-- Supports pruning markers older than the broker's redelivery window, which is
-- the only safe point to discard them.
CREATE INDEX IF NOT EXISTS idx_processed_events_processed_at
    ON modulekit_processed_events (processed_at);

-- Projection rebuild state (ADR 0006 decision 4, ADR 0004 projections).
--
-- rebuilding_since is what separates "this projection is complete" from "this
-- projection is being rebuilt and currently holds partial data". Without it a
-- reader during a rebuild serves incomplete results and has no way to know.
CREATE TABLE IF NOT EXISTS modulekit_projection_state (
    tenant_id        TEXT NOT NULL,
    projection       TEXT NOT NULL,
    updated_at       TIMESTAMP NOT NULL,
    rebuilding_since TIMESTAMP,
    PRIMARY KEY (tenant_id, projection)
);

-- +goose Down
DROP TABLE IF EXISTS modulekit_projection_state;
DROP INDEX IF EXISTS idx_processed_events_processed_at;
DROP TABLE IF EXISTS modulekit_processed_events;
