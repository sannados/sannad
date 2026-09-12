-- +goose Up
-- Webhook endpoints and delivery attempts (ADR 0002).
--
-- ADR 0002 makes webhooks one consumer of the event backbone rather than a
-- separate mechanism, and names five things as table stakes: HMAC signing,
-- retries with exponential backoff, idempotency keys, a dead-letter path, and
-- replay. These two tables carry the last four; signing needs no storage beyond
-- the endpoint secret.
--
-- Both are tenant-scoped. One tenant must not see, edit, or receive another's
-- endpoints, and a delivery names the payload of a tenant's own event.
CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id         TEXT NOT NULL,
    tenant_id  TEXT NOT NULL,
    url        TEXT NOT NULL,
    -- Comma-separated event names. Empty means every event in the tenant, which
    -- an integrator opts into deliberately: it means new event types start
    -- arriving without warning.
    events     TEXT NOT NULL DEFAULT '',
    -- The shared secret. It is the only thing that lets a receiver tell a
    -- genuine delivery from anyone who learned the URL.
    secret     TEXT NOT NULL,
    -- Pausing without deleting is what makes an outage recoverable rather than a
    -- re-registration.
    active     INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_tenant
    ON webhook_endpoints (tenant_id, active);

-- One row per (event, endpoint), not per event. Two endpoints subscribed to the
-- same event fail and retry independently; a shared row would let one slow
-- receiver hold up another.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id               TEXT NOT NULL,
    tenant_id        TEXT NOT NULL,
    endpoint_id      TEXT NOT NULL,
    -- The source event's identity, which travels to the receiver as the
    -- idempotency key. Delivery is at-least-once by construction — a response
    -- lost in transit is indistinguishable from a request never received — so
    -- the receiver needs this to recognise a repeat.
    event_id         TEXT NOT NULL,
    event            TEXT NOT NULL,
    payload          TEXT NOT NULL,
    -- pending | delivered | dead
    status           TEXT NOT NULL DEFAULT 'pending',
    attempts         INTEGER NOT NULL DEFAULT 0,
    -- Backoff is stored rather than held in memory, so a restart does not retry
    -- everything at once — which is how a recovering system finishes off a
    -- receiver that was only briefly down.
    next_attempt_at  TIMESTAMP NOT NULL,
    last_error       TEXT NOT NULL DEFAULT '',
    last_status_code INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL,
    delivered_at     TIMESTAMP,
    PRIMARY KEY (id)
);

-- The dispatcher's own query: everything due, oldest first.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_due
    ON webhook_deliveries (status, next_attempt_at);

-- "Which events did this integration miss" — the first question after an
-- outage, and the reason dead deliveries are kept rather than discarded.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_endpoint
    ON webhook_deliveries (tenant_id, endpoint_id, status);

-- Deduplication support: one delivery per (endpoint, event), so a redelivery
-- from the bus does not queue a second copy.
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_deliveries_unique
    ON webhook_deliveries (endpoint_id, event_id);

-- +goose Down
DROP INDEX IF EXISTS idx_webhook_deliveries_unique;
DROP INDEX IF EXISTS idx_webhook_deliveries_endpoint;
DROP INDEX IF EXISTS idx_webhook_deliveries_due;
DROP TABLE IF EXISTS webhook_deliveries;
DROP INDEX IF EXISTS idx_webhook_endpoints_tenant;
DROP TABLE IF EXISTS webhook_endpoints;
