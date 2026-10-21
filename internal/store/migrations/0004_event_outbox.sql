-- Events booth-api publishes on the event bus (ADR 0123; docs/decisions/0009): api.write.committed,
-- one per committed app write. A row is inserted in the same transaction that records the write's
-- audit entry and idempotency result, and deleted once JetStream has acknowledged it, so an event
-- outlives a bus outage or a restart. msg_id is the JetStream message id, which lets the stream drop
-- a resend inside its duplicate window. Rows older than 24 hours are dropped undelivered. data never
-- holds row values.
CREATE TABLE event_outbox (
    id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    msg_id     TEXT        NOT NULL DEFAULT gen_random_uuid()::text,
    workspace  TEXT        NOT NULL,
    event_type TEXT        NOT NULL,
    data       JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX event_outbox_created ON event_outbox (created_at);
