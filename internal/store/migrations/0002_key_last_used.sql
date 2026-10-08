-- When a key last authenticated a request on a public route (ADR 0101), shown in the key list.
-- Written at most once a minute per key (store.TouchKey).
ALTER TABLE api_keys ADD COLUMN last_used_at TIMESTAMPTZ;
