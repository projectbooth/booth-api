-- Generated APIs (ADR 0100): one per (workspace, catalog dataset), holding the table snapshot REST
-- and GraphQL are built from (internal/apidef).
CREATE TABLE apis (
    id           TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace    TEXT        NOT NULL,
    dataset_id   TEXT        NOT NULL,
    dataset_name TEXT        NOT NULL,
    slug         TEXT        NOT NULL,
    table_schema TEXT        NOT NULL,
    table_name   TEXT        NOT NULL,
    columns      JSONB       NOT NULL,
    primary_key  JSONB       NOT NULL,
    created_by   TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT apis_one_per_dataset UNIQUE (workspace, dataset_id),
    CONSTRAINT apis_slug_unique UNIQUE (workspace, slug)
);

-- API keys (ADR 0100). id is the public part of the key; only SHA-256 of the secret is stored.
-- created_by is the issuing person's `sub`: the owner named on the workspace's workload token
-- (ADR 0103), so it is kept even though the key itself is not tied to a login.
CREATE TABLE api_keys (
    id              TEXT        PRIMARY KEY,
    workspace       TEXT        NOT NULL,
    name            TEXT        NOT NULL,
    secret_hash     BYTEA       NOT NULL CHECK (length(secret_hash) = 32),
    created_by      TEXT        NOT NULL,
    created_by_name TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ,
    revoked_by      TEXT
);
CREATE INDEX api_keys_workspace ON api_keys (workspace);

-- A key's scope: the datasets (and so the generated APIs) it may read. Keyed by dataset rather
-- than API row so regenerating an API keeps its keys; deleting an API removes these rows, so a
-- later API for the same dataset never silently re-enables old keys.
CREATE TABLE api_key_datasets (
    key_id     TEXT NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    dataset_id TEXT NOT NULL,
    PRIMARY KEY (key_id, dataset_id)
);
CREATE INDEX api_key_datasets_dataset ON api_key_datasets (dataset_id);
