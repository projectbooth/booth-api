# Contract: events booth-api publishes

Version: v0 (ADR 0123 and its 2026-10-21 revision; booth-api `docs/decisions/0009`). Owned by
`booth-api`. Written for `booth-pipeline`, whose event triggers consume it, and for anyone else
reading booth-api's events off the bus.

booth-api publishes one event type, `api.write.committed`, on booth-core's event bus (NATS JetStream,
ADR 0021). It uses the shared `BOOTH_EVENTS` stream and the fixed envelope (ADR 0026).

## `api.write.committed`

One event per app write whose transaction committed (`docs/contracts/app-writes.md`'s app write
route), REST or GraphQL.

```
subject   booth.<workspace>.api.write.committed
header    Nats-Msg-Id: api-<uuid>           (one per event; resends inside the stream's duplicate window are dropped)
```

```json
{
  "workspace": "acme",
  "eventType": "api.write.committed",
  "publishedAt": "2026-10-21T18:00:00.123456Z",
  "publishedBy": "api",
  "data": {
    "apiSlug": "finance-dev-transactions",
    "dataset": "6f1c2e0a-…",
    "table": "finance.transactions",
    "operation": "update",
    "rows": 1,
    "principal": "streamlit:acme:budget"
  }
}
```

| Field | Meaning |
|---|---|
| `workspace` | The API's workspace, which is also the writing app's: taken from its verified token, never from a header. |
| `publishedAt` | When booth-api published the event. It is later than the commit if the event waited out a bus outage. |
| `data.apiSlug` | The API's slug, unique in its workspace. Match triggers on this. |
| `data.dataset` | The API's catalog dataset id. |
| `data.table` | The API's table as `schema.name`, from its stored snapshot. |
| `data.operation` | `insert`, `update` or `delete`. |
| `data.rows` | How many rows the request changed. Always `1` today (one row per request); kept for a later bulk path. |
| `data.principal` | The writing app's token subject, `streamlit:<workspace>:<appId>`. Never a person, and never a pipeline run (a run can't write through booth-api today). |

**Never in an event:** row values, primary-key values (or their hash), the user the app reported, the
idempotency key, or the owner whose grant the write used. A test enforces this. Those details stay in
booth-api's write audit log, which owners read on the API page.

## When an event is published

| Write | Event |
|---|---|
| Committed: `201` insert, `200` update or delete, including after booth-api's one reissue on `42501` | one |
| A replay of an idempotency key that already committed | none |
| A refusal (`400`, `403`, `404`, `409`, `412`, `422`, `429`), a timeout, a rollback | none |
| The connection failed at `COMMIT`, so the outcome is unknown (`503 idempotency_outcome_unknown`) | none (ADR 0123 revision) |
| Changes to write settings or grants | none |
| Rows written straight to PostgreSQL (a pipeline task, a lease), not through booth-api | none: booth-api never sees them |

## Delivery

- **At least once.** A consumer must tolerate duplicates and reordering. Resends inside the stream's
  duplicate window (2 minutes by default) carry the same `Nats-Msg-Id` and are dropped by JetStream.
- **Outages.** The event is recorded in booth-api's own database, in the same transaction as the
  write's audit entry, and published from there:
  - when the bus is down, it is published once the bus is back;
  - after a restart, it is published by whichever replica runs first.
- **Undelivered events are dropped after 24 hours**, logged by booth-api.
- **What can still lose an event:**
  - booth-api's own database failing in the moment between the data commit and recording the write
    (the audit entry is lost with it);
  - an outage longer than 24 hours.

  A pipeline that must not miss a write keeps a slow interval or cron trigger as well (ADR 0123
  point 6).
- **Events say that something changed, not what it is now.** A consumer reads the table itself;
  `booth-pipeline` hands the events to a run as a hint only.

## Permissions

- **Publishing.** booth-api's manifest declares `events: {publish: [api.write.committed]}`. booth-core
  then gives it a credential that can publish only `booth.*.api.write.committed` (ADR 0049, 0050).
- **Subscribing.** A consumer declares `events: {subscribe: [api.write.committed]}` on its own
  manifest.
- **Turning it off.** The chart's `eventBus.enabled=false` leaves booth-api off the bus. Writes then
  work as before and publish nothing.
