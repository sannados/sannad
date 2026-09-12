-- +goose Up
-- Saga coordinator durable state (ADR 0006 decision 4).
--
-- ADR 0006 forbids a transaction spanning two modules, so a business operation
-- touching several of them has no single unit of work to roll back. A saga
-- sequences the steps and undoes the committed ones when a later step fails.
--
-- The coordinator can crash between any two steps, so this state is not a log
-- written for observability — it is the only thing that tells a restarted
-- process how far the operation got. Every transition is written before the
-- step it describes runs.
--
-- The PRIMARY KEY on (tenant_id, saga_id) is a mechanism, not a lookup
-- optimisation. A saga ID is tied to whatever triggered the operation, so a
-- second insert with the same ID is a redelivery of that trigger. The key is
-- what makes the redelivery fail instead of starting a second copy of an
-- in-flight saga, and it is also what guarantees a single coordinator per
-- instance, which the attempt counter's read-then-write depends on.
CREATE TABLE IF NOT EXISTS modulekit_sagas (
    tenant_id      TEXT NOT NULL,
    saga_id        TEXT NOT NULL,
    name           TEXT NOT NULL,
    status         TEXT NOT NULL,
    failure_reason TEXT NOT NULL DEFAULT '',
    started_at     TIMESTAMP NOT NULL,
    updated_at     TIMESTAMP NOT NULL,
    PRIMARY KEY (tenant_id, saga_id)
);

-- Supports the startup recovery sweep, which asks for every saga still running
-- or compensating. Without it that sweep scans every saga ever run.
CREATE INDEX IF NOT EXISTS idx_sagas_status
    ON modulekit_sagas (tenant_id, status);

-- Per-step state.
--
-- position is part of the key, and is what compensation orders by. It is stored
-- rather than derived from the step definitions because a deploy between a
-- failure and its recovery could reorder the definitions, and compensating in
-- the new order would run one step's undo against another step's effect.
--
-- attempts counts entries into the step body. A step found mid-flight on
-- recovery is re-entered, and this is what makes a step that fails the same way
-- every time visible instead of an invisible loop.
CREATE TABLE IF NOT EXISTS modulekit_saga_steps (
    tenant_id      TEXT NOT NULL,
    saga_id        TEXT NOT NULL,
    position       INTEGER NOT NULL,
    name           TEXT NOT NULL,
    status         TEXT NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 0,
    failure_reason TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMP NOT NULL,
    PRIMARY KEY (tenant_id, saga_id, position)
);

-- +goose Down
DROP TABLE IF EXISTS modulekit_saga_steps;
DROP INDEX IF EXISTS idx_sagas_status;
DROP TABLE IF EXISTS modulekit_sagas;
