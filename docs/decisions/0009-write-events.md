# 0009: Publishing api.write.committed (ADR 0123)

Date: 2026-10-21. Status: **proposed** (for coordinator review with the PR).

Built from my design note (`docs/design-event-triggers.md`, on the local branch
`design-event-triggers`, 8fe23b7), with ADR 0123's 2026-10-21 revision overruling it where they
differ. The wire contract for consumers is `docs/contracts/events.md`.

## What was built

| Piece | Where |
|---|---|
| The outbox table (migration 0004) | `internal/store/migrations/0004_event_outbox.sql` |
| Recording a committed write in one transaction: audit entry, idempotency result, event | `internal/store` (`RecordWrite`), `internal/writes` (`recordCommitted`) |
| Claiming, publishing and expiring queued events | `internal/store/outbox.go`, `internal/events/relay.go` |
| The bus connection (a copy of booth-database's, itself booth-streamlit's) | `internal/events/bus.go` |
| `BOOTH_NATS_URL`, `BOOTH_NATS_CREDS_FILE` | `internal/config`, `cmd/api` |
| Chart: `eventBus.*`, `nats.url`, the manifest's `events.publish`, the credential mount | `charts/booth-api` |

## How it follows the rulings

- **Once per committed request.**
  - `writes.Service.Execute` marks a write committed only when `table.Write` returned without
    error, which means its `COMMIT` succeeded.
  - Only then does `recordCommitted` run. In one transaction in booth-api's own database it writes
    the audit entry, the idempotency key's `done` and the outbox row.
  - Before this change, the audit entry and the key were two separate calls.
- **Nothing is published for:**
  - a replay, an in-progress or interrupted key, or any refusal;
  - a rate-limited write, a missing row or a stale version;
  - an unknown outcome (ruled);
  - settings and grant changes.
- **`rows` is always 1** (one row per request). The 42501 reissue still gives one event.
- **Payload** (ruled): `apiSlug`, `dataset` (the catalog id), `table` (`schema.name`), `operation`,
  `rows`, `principal`. A test writes marker values and checks that no event holds a row value, a
  key value, the reported user, the idempotency key or `grantedBy`.
- **The outbox, kept 24 hours.**
  - Every replica runs a relay. It is woken after each queued event and otherwise looks every 5
    seconds.
  - It claims up to 100 rows with `FOR UPDATE SKIP LOCKED` and publishes each one with a JetStream
    ack. `Nats-Msg-Id` is `api-<msg_id>`.
  - Each row is deleted once acknowledged. The relay stops at the first failure, so order is kept.
  - Rows older than 24 hours are dropped, with the line `events: dropped N undelivered event(s)
    older than 24h0m0s from the outbox`.
- **The bus never blocks a write.** With the bus down the write answers as usual. Its event waits
  in the outbox and is published once the bus reconnects (nats.go retries forever).

## Departures from my note: please review

1. **The pod waits for core's credential Secret**, like booth-database's. My note said "mounted as
   optional; if absent, events are off". I changed this to match the fleet:
   - a pod that started without the Secret would queue events nobody publishes and then drop them
     after 24 hours;
   - core writes the Secret within seconds of seeing the manifest.
   - `eventBus.enabled=false` is the way to run without the bus.
2. **`publishedAt` is when the relay published the event, not the commit time.** The envelope
   field says "published", and an event that waited out an outage would otherwise look older than
   it was sent. The payload has no commit time; the ADR fixes the fields.
3. **If recording the committed write fails** (booth-api's database fails right after the data
   commit), booth-api records the audit entry alone and logs that the event is lost. The key stays
   pending, as it did before when its "done" couldn't be recorded. This is the window the note
   accepted.

## Tests

- **`internal/writes` (`TestEventsForCommittedWritesOnly`):**
  - one event, with exact data, for each insert (with a key), update, delete, and an insert after a
    42501 reissue;
  - none for a replay, a non-writable column, a constraint, a missing row, a stale version, an
    ungranted operation, the rate limit, or events off;
  - the key is `done` in the same transaction;
  - the relay is woken once per event;
  - the marker-value check.
- **`internal/events`**, against the test PostgreSQL and an in-process nats-server with
  `BOOTH_EVENTS`:
  - publish with subject, envelope, data and message id; a same-id resend is stored once;
  - with the bus down, the event stays queued and is published once after the bus returns;
  - `Wake` publishes at once;
  - the first failure stops a pass and keeps order;
  - two replicas never send the same row;
  - 24-hour expiry.
- **Not covered by a unit test: the unknown-outcome case.** I can't make PostgreSQL fail exactly at
  `COMMIT` in a test. That path returns an error, and an event is recorded only on the committed
  path.
- **Contract:**
  - the manifest declares exactly `publish: [api.write.committed]`;
  - the credential is a directory mount from core's Secret, not optional;
  - `eventBus.enabled=false` renders nothing of the bus;
  - a stand-in bus without a Secret needs `nats.url`.
- **kind:** the real CRD keeps `events.publish`. With a stand-in Secret pointing at no bus, the pod
  still becomes ready, and migration 0004 creates `event_outbox`.
- **realstack (step 8f):**
  - the stand-in streamlit module declares `events.subscribe: [api.write.committed]`, so core gives
    the check a real subscriber credential;
  - after step 8, exactly 7 events are read off core's bus (6 inserts and 1 update; budget 6, other
    1), with the right subject, envelope, data and message id;
  - none for the replay, the refusals, or the lease's own insert in 8c (a direct PostgreSQL write,
    which the ADR doesn't cover);
  - no row values, users, keys or owner `sub` in any event;
  - booth-api's log shows `event bus connected` with the credential core minted from its manifest.
