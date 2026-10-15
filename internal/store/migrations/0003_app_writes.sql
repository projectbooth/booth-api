-- Writes through booth-api (ADR 0117; docs/decisions/0008). Nothing here is read on the read path.

-- Snapshots taken from version 2 on carry what writes need to know about each column (defaults,
-- identity, generated). An API generated before that must be regenerated before writes go on.
ALTER TABLE apis ADD COLUMN snapshot_version INT NOT NULL DEFAULT 1;

-- An API's write settings: no row means writes are off. Operations and columns are what an owner
-- turned on for the whole API; a grant can only narrow them. version increases on every save, so
-- a write sidecar started under older settings is replaced rather than renewing a stale scope.
CREATE TABLE api_writes (
    api_id     TEXT        PRIMARY KEY REFERENCES apis (id) ON DELETE CASCADE,
    operations TEXT[]      NOT NULL CHECK (operations <@ ARRAY['insert', 'update', 'delete'] AND cardinality(operations) > 0),
    columns    TEXT[]      NOT NULL,
    version    BIGINT      NOT NULL DEFAULT 1,
    updated_by TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Write grants. principal is the literal subject the caller's token carries:
-- streamlit:<workspace>:<appId> for an app (the only kind built), or key:<id> for a future write
-- key. columns NULL means "every column the API makes writable", so it follows the API's settings;
-- a list narrows them. granted_by is the owner whose access the write lease is minted under.
CREATE TABLE write_grants (
    id         TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace  TEXT        NOT NULL,
    api_id     TEXT        NOT NULL REFERENCES apis (id) ON DELETE CASCADE,
    principal  TEXT        NOT NULL CHECK (principal ~ '^(streamlit:[a-z0-9-]+:.+|key:.+)$'),
    operations TEXT[]      NOT NULL CHECK (operations <@ ARRAY['insert', 'update', 'delete'] AND cardinality(operations) > 0),
    columns    TEXT[],
    granted_by TEXT        NOT NULL,
    granted_by_name TEXT   NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    revoked_by TEXT
);
-- At most one live grant per API and principal: granting again replaces it.
CREATE UNIQUE INDEX write_grants_live ON write_grants (api_id, principal) WHERE revoked_at IS NULL;
CREATE INDEX write_grants_workspace_principal ON write_grants (workspace, principal);

-- Idempotency keys, kept 24 hours. response is the write's result (status, row, version), so a
-- replay answers exactly as the first request did; state 'pending' marks a write in flight, or one
-- whose outcome a crash left unknown.
CREATE TABLE write_idempotency (
    api_id       TEXT        NOT NULL,
    principal    TEXT        NOT NULL,
    key          TEXT        NOT NULL,
    request_hash TEXT        NOT NULL,
    state        TEXT        NOT NULL CHECK (state IN ('pending', 'done')),
    response     JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (api_id, principal, key)
);
CREATE INDEX write_idempotency_created ON write_idempotency (created_at);

-- The write audit log, kept 90 days by default (writes.auditRetention). Row values are never
-- recorded: row_key_hash is SHA-256 of the row's primary key, so an investigator holding a key can
-- confirm whether that row was touched without the log holding the key itself.
CREATE TABLE write_log (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    workspace       TEXT        NOT NULL,
    api_id          TEXT        NOT NULL,
    operation       TEXT        NOT NULL,
    principal       TEXT        NOT NULL,
    granted_by      TEXT        NOT NULL,
    reported_user   TEXT        NOT NULL DEFAULT '',
    row_key_hash    TEXT        NOT NULL DEFAULT '',
    idempotency_key TEXT        NOT NULL DEFAULT '',
    status          INT         NOT NULL,
    outcome         TEXT        NOT NULL,
    duration_ms     INT         NOT NULL
);
CREATE INDEX write_log_at ON write_log (at);
CREATE INDEX write_log_api ON write_log (workspace, api_id, at);

-- Changes to write settings and grants, with who made them (kept with the write log's retention).
CREATE TABLE write_admin_log (
    id        BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    workspace TEXT        NOT NULL,
    api_id    TEXT        NOT NULL DEFAULT '',
    actor     TEXT        NOT NULL,
    action    TEXT        NOT NULL,
    detail    JSONB       NOT NULL DEFAULT '{}'
);
CREATE INDEX write_admin_log_at ON write_admin_log (at);
