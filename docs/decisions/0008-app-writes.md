# 0008: App writes through booth-api (ADR 0117)

Date: 2026-10-15. Status: **accepted** (coordinator review of PR #17, 2026-10-15). The `onBehalfOf`
gap (departure 1) is accepted as is: booth-database's log shows the lease and the granting owner,
and booth-api's write audit log holds the app and the reported user.

Built from my design note (`docs/design-app-writes.md`, on the local branch `design-app-writes`),
with ADR 0117's "Reconciled design" overruling it where they differ. The wire contract for other
modules is `docs/contracts/app-writes.md`.

## What was built

| Piece | Where |
|---|---|
| Write settings and grants, idempotency keys, the audit log (migration 0003) | `internal/store` |
| Validation against the snapshot, the SQL, SQLSTATE mapping, row versions | `internal/table/write.go` |
| Settings, grants, the app write path, the reissue on 42501 | `internal/writes` |
| App token verification against core's workload issuer | `internal/appauth` |
| `/app/v1/` REST and GraphQL | `internal/appwrite`, `internal/gql/mutation.go` |
| booth-api's own broker calls (full scope, revoke) | `internal/broker` |
| Per-app rate limit | `internal/ratelimit` |
| Write sidecars per (API, granting owner) | `internal/sidecars` |
| `apiwrite:` tokens at roleCeiling editor | `internal/workload` |
| Owner-only management routes | `internal/api` |
| Chart: `oidc.workloadIssuerUrl` (in the identity-keys annotation), `writes.*` | `charts/booth-api` |

## How it follows the rulings

- **Owners only.** Turning writes on, changing them, turning them off, the write log and every
  grant route need the owner role. Reading an API's write settings needs any role.
- **Operations.** Insert is the default, and so is a grant's default. Update and delete are opt-in
  per API. A grant can narrow the API's operations and columns, never widen them.
- **Apps first.** The grants table's principal check accepts `key:<id>`, but the API refuses it
  (`422`). The public route stays read-only.
- **The principal is the literal app subject**, `streamlit:<workspace>:<appId>`, taken from the token.
  - The token is verified against `oidc.workloadIssuerUrl` (go-oidc discovery on core's issuer).
  - Its `booth_module` claim must be `streamlit`.
  - Its single workspace group must match the subject's workspace.
  - App tokens are accepted on `/app/v1/` only. The management API trusts only the OIDC issuer, so
    an app token gets `401` there; the public route takes API keys only.
  - `/app/v1/` serves no reads: `GET` is `405`, and a GraphQL query operation is refused.
- **The write sidecar per (API, granting owner):**
  - It uses subject `apiwrite:<api-id>`, `roleCeiling: editor`, the granting owner as `owner`,
    `--access=tablewrite`, and `--scope={"workspace", "write": {"grant": "api:<api-id>", "tables": [...]}}`.
    That scope is exactly booth-database's §1 shape.
  - booth-api refuses a minted write token that came back below editor, the same as a refusal.
  - The sidecar starts on the first write and stops after `dataAccess.idle`.
  - It also stops at once when:
    - writes are turned off;
    - the settings change (it is replaced, never renewed with a stale scope);
    - the owner's last grant on the API is revoked.
- **Full-scope request on every settings save, made before the settings are stored.** If
  booth-database refuses the scope, the save is refused with its message. So the stored settings
  never claim more than the database grants.
- **Revoke request when writes are turned off** (`options.revoke`, `tables: []`). It is also sent
  when an owner deletes a writable API.
- **One reissue on 42501.**
  1. The full scope is sent again, as the granting owner and on behalf of the app.
  2. The (API, owner) sidecar is restarted.
  3. The write is retried once.
  - A second 42501 is `503`.
- **Limits:**
  - 10 writes per second with a burst of 50, per principal and per pod.
  - One row per request, 64 KiB at most.
  - The audit log is kept 90 days. Row values are never logged; the realstack test greps booth-api's
    log for a marker value to check this.
- **Idempotency and optimistic concurrency** work as in the note (§4); the contract has the
  details.

## Departures from the brief or the note: please review

1. **`onBehalfOf` only reaches the broker on booth-api's own calls, not the sidecar's lease
   requests.**
   - booth-core's `credential-sidecar` (v0.3.5) has no flag for `onBehalfOf`, and I can't change
     core.
   - One write sidecar per (API, granting owner) serves every app that owner granted, so a
     per-sidecar value would also be wrong.
   - So:
     - the 42501 reissue carries the app's subject;
     - a settings save and the revoke request carry the acting owner's `sub`;
     - the sidecar's own issuances carry none.
   - booth-api's audit log records the app for every write, so the person and the app are still on
     record. Only booth-database's line for a sidecar issuance lacks it.
   - If you want it there as well, core needs a `--on-behalf-of` sidecar flag, and booth-api would
     need a sidecar per (API, owner, app). Say if you want that requested.
2. **The credential-sidecar digest moved** to the image published from core `a562691` (v0.3.5,
   `sha256:a5bb9dee…9907`). The old pin (`6a0a795e…`) refuses `--access=tablewrite`; I checked by
   running both. The Dockerfile's old comment said to bump it with booth-pipeline and
   booth-notebooks. I didn't: their read and readwrite behaviour is unchanged, and I can't edit
   those repos.
3. **A body over 64 KiB is `413`**, not the note's `400` (the code is still `invalid_request`).
4. **GraphQL names.**
   - The note had `insertX` / `updateX` / `deleteX` returning `X!`.
   - Built: `insertRow` / `updateRow` / `deleteRow` returning `RowWrite { row, version }`, so a
     client gets the version without a second request. The input types are `RowInsert`,
     `RowPatch` and `RowKey`.
5. **The audit log stores a hash of the row's key, not the key itself.** A primary key is a row
   value (an email, for example), and the ruling is that row values are never logged.
6. **Idempotency records hold the write's response, row values included, for 24 hours,** in
   booth-api's own database, so a replay can answer exactly as the first request did. This is a
   cache, not a log, and it is pruned hourly. If you'd rather it hold no values, a replay would
   return only the key and version.
7. **Deleting a writable API needs an owner.** An editor can still delete a read-only API, as before.
8. **A grant for an app that already has one on the API replaces it,** rather than answering `409`.
   booth-streamlit's "save" is then one call.
9. **Writes need a version-2 snapshot.**
   - Snapshots now record each column's default, identity and generated flags; the write
     validation and the writable-candidate rules need them.
   - An API generated before this change must be regenerated before writes can be turned on (`409`
     with a clear message). Its reads are unaffected.

## Test coverage

- **Unit, against real PostgreSQL:**
  - A restricted role stands in for booth-database's write group. The fake broker grants it exactly
    what each scope names, so a write outside the settings really fails with 42501, and the reissue
    really fixes it.
  - The rest:
    - settings validation and the order of a save;
    - grants (narrowing, replacing, workspace isolation);
    - insert/update with versions;
    - every validation and constraint code;
    - idempotency replay, mismatch, release, in-flight and unknown;
    - the rate limit;
    - revoke and revoke-principal (sidecars stopped only when an owner's last grant goes);
    - turning writes off (the revoke request, grants not restored);
    - the granting owner refused at mint;
    - regenerate never widening;
    - the audit log holding no values.
  - The HTTP handler tests cover:
    - authentication (no token, a non-app token, another workspace's app with forged headers);
    - no reads;
    - REST and GraphQL;
    - `If-Match`;
    - the 64 KiB limit;
    - `Retry-After`.
  - The management tests cover owners only, and app tokens refused (`401`).
  - The write-sidecar tests cover flags, mint identity, version replacement and stops. They run on
    Linux, as CI does.
- **realstack (step 8 of `0007`):** real booth-core v0.3.5, booth-database v0.1.4, real
  sidecars, and real app tokens minted with a stand-in `streamlit` module's credential. It checks:
  - owners only;
  - insert, update, `If-Match`, GraphQL and idempotency;
  - app tokens refused on reads, management and the public route;
  - a read key can't write;
  - another workspace's app gets `404`;
  - a lease for the same scope can't write another table, another column, delete, or run DDL;
  - removing the granting owner fails the next mint;
  - revoking stops writes;
  - revoke-principal;
  - the audit log has no values;
  - turning writes off ends the lease;
  - booth-api's log shows the write sidecars starting and stopping, with no row value in it.

## Amendment (ADR 0117 item 1a, 2026-10-15)

booth-streamlit never holds a person's token, so owners grant on booth-api's own page and
booth-streamlit only links there. Built:

1. **The revoke-only service path.** `POST /api/write-grants/revoke-principal` also accepts
   booth-streamlit's grants service token: subject exactly `streamlit:<workspace>:grants`,
   `booth_module: streamlit`, verified against core's workload issuer, with the workspace from the
   token. `internal/api` serves this one route outside the people-only group (`serviceOr`).
   - **What it can do.** It can revoke only that workspace's app principals. `:sources`, `:grants`,
     other workspaces and `key:` are refused.
   - **What it can't do.** Every other route refuses it (`401`), because they verify only the OIDC
     issuer.
   - **Reserved ids.** `grants` joins `sources` as a reserved appId: never an app token, never a
     principal.
   - **Testing.** Unit tests cover each refusal and the idempotent second call. realstack mints the
     service token with the stand-in streamlit module's credential and uses it for the take-ownership
     revoke.
2. **The grant form on the Writes panel.**
   - It takes a principal, the operations (only those the API allows), and optional column
     narrowing.
   - It lists the live grants, each with Revoke.
   - It reads `?writeApi=<slug or id>&writePrincipal=<principal>` from booth-streamlit's link (see
     `docs/contracts/app-writes.md`).
