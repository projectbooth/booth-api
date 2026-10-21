# Contract: app writes through booth-api

Version: v0 (ADR 0117, its "Reconciled design" and item 1a; booth-api `docs/decisions/0008`). Owned by
`booth-api`. Written for `booth-streamlit` (the "Write targets" panel, the write proxy, take-ownership)
and `booth-storage` (whose upload grants follow the same shape), and for anyone building against
either.

Three surfaces:

1. **The grant management API**: owner-only routes on booth-api's ordinary management API, called
   with a person's own sign-in token from booth-api's own page in the shell (the Writes panel).
2. **The revoke-only service path**: `revoke-principal` also accepts booth-streamlit's own grants
   service token, so take-ownership can revoke an app's grants (ADR 0117 item 1a).
3. **The app write route**: what an app's Streamlit gate forwards an app's write to, with the
   app's own workload token.

Each committed write also publishes `api.write.committed` on the event bus, for pipeline event
triggers (ADR 0123; `docs/contracts/events.md`).

booth-api holds the grants (ADR 0117 "Reconciled design" item 1). booth-streamlit never holds a
person's token, so it doesn't make grants (item 1a):
- The owner grants on booth-api's Writes panel. booth-streamlit's "Write targets" panel links there
  with the app filled in (see "Linking to the grant form").
- booth-streamlit keeps the app-side declaration for the app header and early refusal.
- A write needs booth-streamlit's declaration **and** booth-api's live grant.

## Principals

A grant names the **literal subject the caller's token carries**:

| Principal | Who | Status |
|---|---|---|
| `streamlit:<workspace>:<appId>` | a booth-streamlit app's workload token (`<appId>` matches `[A-Za-z0-9._-]{1,128}`) | built |
| `key:<keyId>` | a write-enabled API key on the public route | **not built**: the grants table accepts the shape, the API refuses it with `422` |

`<workspace>` must be the workspace the grant is made in. `streamlit:<workspace>:sources` (catalog
listing) and `streamlit:<workspace>:grants` (the grants service) are booth-streamlit's own
identities. They are never valid principals, and never accepted as app tokens.

## 1. Management API

Base: `/modules/api/api` through core's gateway (booth-api serves it as `/api`).
Headers: `Authorization: Bearer <person's OIDC token>`, `X-Workspace: <slug>`.
Errors: `{"error": "<message>"}`, plus `"problems": [{"field", "problem"}]` on a `422` settings
refusal. Every route below needs the **owner** role in the workspace (`403` otherwise), except
`GET .../writes`. Anything else, including an app's workload token, gets `401`.

### API write settings

An API maps one catalog dataset (one table), so "per API" means "per dataset".

```
GET    /api/apis/{apiId}/writes            any role
PUT    /api/apis/{apiId}/writes            owner      {"operations": [...], "columns": [...]}
DELETE /api/apis/{apiId}/writes            owner
GET    /api/apis/{apiId}/writes/log        owner      ?limit=1..1000 (default 100)
```

`GET` returns:

```json
{"apiId": "…",
 "writes": null | {"operations": ["insert"], "columns": ["amount", "entered_by"], "version": 3,
                   "updatedBy": "<sub>", "updatedAt": "…"},
 "columns": [{"column": "id", "field": "id", "type": "bigint", "writable": false,
              "reason": "a primary-key column the database fills in", "required": false}, …],
 "hasPrimaryKey": true, "needsRegenerate": false, "dataAccess": true}
```

`writes: null` means writes are off (the default). `columns` lists every column in the snapshot,
whether it can be made writable (`reason` if not), and whether an insert must set it.

`PUT` turns writes on or changes them; it returns the same view.
- **`operations`** is a subset of `insert`, `update` and `delete`, and can't be empty. Insert is
  the usual choice. Update and delete are opt-in, and need a primary key.
- **`columns`** are real column names, each a writable candidate. A writable candidate is not an
  array, not generated, not an identity column, and not a primary-key column with a default. If
  `insert` is on, every column an insert must set has to be listed. Delete alone takes no columns.
- **Order of a save:**
  1. booth-api first sends the full `tablewrite` scope to core's broker, as the saving owner.
     booth-database reissues the API's write group to exactly these settings, or refuses them. A
     refusal is `422` with booth-database's message, and nothing is saved.
  2. Then the settings are stored and their `version` goes up by one.
  3. Then every write sidecar for the API is restarted.
- **Status codes:**
  - `409`: the API was generated before writes existed; regenerate it first.
  - `503`: booth-api has no data access (`core.url` unset).

`DELETE` turns writes off:
- In one transaction, it removes the settings and revokes **every** grant on the API.
- It then stops the API's write sidecars.
- It then sends the revoke request (`options.revoke`, `tables: []`), which ends every live lease of
  the API's write group in the database.
- It answers `{"revokedGrants": n, "databaseRevoked": true|false, "databaseError"?: "…"}`. A failed
  revoke request is reported, not fatal: nothing in booth-api can use the group any more, and
  booth-database's reaper drops an empty group.
- Turning writes on again never brings old grants back.

`GET .../log` returns `{"items": [{"at", "operation", "principal", "grantedBy", "reportedUser"?,
"rowKeyHash"?, "idempotencyKey"?, "status", "outcome", "durationMs"}]}`, newest first. Row values
are never recorded. `rowKeyHash` is the SHA-256 of the row's primary key: someone who has a key can
check whether that row was touched, without the log holding the key itself.

**Regenerating never widens what can be written:**
- A column added to the table stays read-only until an owner ticks it.
- A column dropped from the table leaves the settings, and the settings' `version` goes up.

**Deleting a writable API:** an editor gets `409`. An owner turns writes off first, and the delete
does that automatically.

### Grants

```
GET  /api/write-grants                      owner   ?apiId=&principal=&includeRevoked=true
POST /api/write-grants                      owner   {"apiId", "principal", "operations"?, "columns"?}
POST /api/write-grants/{grantId}/revoke     owner
POST /api/write-grants/revoke-principal     owner, or booth-streamlit's grants service   {"principal"}
```

A grant:

```json
{"id": "…", "apiId": "…", "principal": "streamlit:acme:a2b3c4", "operations": ["insert"],
 "columns": null, "grantedBy": "<owner sub>", "grantedByName": "…", "createdAt": "…",
 "revokedAt": null, "revokedBy"?: "<sub>"}
```

- **`POST /api/write-grants`** creates a grant (`201`).
  - `operations` defaults to `["insert"]`.
  - `columns: null` (or absent) means every column the API's settings make writable, so the grant
    follows later changes to the settings. A list narrows to those columns.
  - Both can only narrow the API's settings. Widening them is `422`.
  - **A second grant for the same app and API replaces the first**, which is revoked in the same
    transaction. So changing an app's grant on the Writes panel is just this call.
  - Refusals:
    - `409`: writes are off for the API.
    - `400`: the principal isn't an app in this workspace.
    - `422`: the principal is `key:…`.
    - `404`: the API isn't in this workspace.
  - **`grantedBy` matters.** The grant's write lease is minted under that owner's access. If that
    owner loses the editor or owner role in the workspace, or hasn't signed in within core's 7-day
    window, the app's writes stop with `403 granting_owner_no_access` until an owner grants again.
- **`.../{grantId}/revoke`** revokes one grant and returns it. Revoking twice is not an error. The
  granting owner's write sidecar for that API stops at once if that was their last grant on it.
- **`revoke-principal`** revokes every live grant the principal holds in the workspace, across every
  API, in one statement. It answers `{"revoked": n, "items": [grants]}`. **booth-streamlit calls
  this on take-ownership, as part of the same step as the owner change, and must fail closed** if
  booth-api can't be reached or answers non-2xx: the owner change doesn't happen. It is idempotent
  (`revoked: 0` the second time).

### The revoke-only service path (ADR 0117 item 1a)

booth-streamlit makes the take-ownership call with a workload token it mints for itself through
core: subject exactly `streamlit:<workspace>:grants`, roleCeiling viewer (revoking only ever reduces
privilege). It sends the token through core's gateway as usual:

```
POST /modules/api/api/write-grants/revoke-principal
Authorization: Bearer <grants service token>      X-Workspace: <workspace>
{"principal": "streamlit:<workspace>:<appId>"}
```

- **Verification.** booth-api verifies the token against core's **workload issuer**
  (`oidc.workloadIssuerUrl`). It must carry `booth_module: streamlit` and core's single workspace
  group, for the same workspace as the subject. The workspace is taken from the token; headers are
  ignored.
- **What it may revoke.** Only principals `streamlit:<that workspace>:<appId>`:
  - another workspace's app, `:sources` or `:grants` → `400`;
  - `key:…` → `422`.
- **Responses.** The same as for an owner: `{"revoked": n, "items": [...]}`. The second call answers
  `revoked: 0`. Each grant's `revokedBy` is the service's subject.
- **Everywhere else, the token is refused.** That includes creating, listing or revoking a single
  grant, write settings, the write log, APIs and keys. The people's verifier answers `401`. An app's
  own token (`streamlit:<ws>:<appId>`) is refused here too (`401`).
- **booth-api must have `oidc.workloadIssuerUrl` set.** Without it this path is off, and only owners
  can call `revoke-principal`.

### Linking to the grant form

booth-streamlit's "Write targets" panel links an owner to booth-api's page with the grant prefilled:

```
/apis?writeApi=<API slug or id>&writePrincipal=streamlit:<workspace>:<appId>
```

- **The parameters.** Both are URL-encoded. `writeApi` matches the API's slug, which booth-streamlit
  stores in its declaration, or its id.
- **For an owner,** the page opens that API's Writes panel with the principal filled in. Choosing
  operations and columns and pressing Grant still needs the owner. Nothing is granted by the link
  alone.
- **For an editor or viewer,** the page says only an owner can grant.
- **For an unknown API,** the page says so.
- **When writes are off for the API,** the owner must turn them on first; the panel says so.

## 2. The app write route

booth-streamlit's gate forwards an app's write to core's ordinary gateway:

```
POST|PATCH|DELETE /modules/api/app/v1/{slug}/...
Authorization: Bearer <the app's own workload token, unchanged>
X-Workspace: <the app's workspace>
X-Booth-App-User: <the signed-in person the gate saw>          optional, audit only
Idempotency-Key: <1-255 chars>                                    optional
If-Match: "<version>"                                             optional, update/delete
```

**Authentication:**
- The token must be the app's existing viewer-capped workload token: subject
  `streamlit:<workspace>:<appId>`, `booth_module: streamlit`, one workspace group.
- booth-api verifies it against core's **workload issuer** (`oidc.workloadIssuerUrl`). The workspace
  comes from the token and never from a header.
- No grant for this app on this API means `403`.
- App tokens are refused everywhere else: `401` on the management API and on the public key route.
  There are no reads here: `GET` is `405`, and a GraphQL query is refused.

**The `{slug}`** is the API's slug, shown on the API page. A slug from another workspace is `404`.

### REST

```
POST   /app/v1/{slug}/rows               {"data": {field: value, …}}     201  {"data": row, "version"}
PATCH  /app/v1/{slug}/rows/{key}         {"data": {field: value, …}}     200  {"data": row, "version"}
DELETE /app/v1/{slug}/rows/{key}                                         200  {"data": deletedRow, "version"}
PATCH|DELETE /app/v1/{slug}/rows?key[field]=value&…                      composite keys, or keys containing "/"
```

- **Writes:**
  - One row per request; the body is at most 64 KiB (`413` above that).
  - `field` names are the API's field names, the same as reads use.
  - Values use the read path's formats: `bigint` and `numeric` as strings, ISO 8601 dates and
    times, `null` for SQL NULL.
- **Responses:**
  - The response carries every field, in column order, as a read would return it.
  - The response also carries the row's `version`, which is SHA-256 of its whole `to_jsonb`
    rendering. An app reading the table directly can compute the same value with
    `encode(sha256(convert_to(to_jsonb(t.*)::text, 'UTF8')), 'hex')`.
  - `ETag: "<version>"` is set on every success.
  - An insert sets `Location` when the table has a one-column primary key.
- **With `If-Match`**, an update or delete happens only if the row is still at that version
  (`412` otherwise).
- **Without `If-Match`**, the last write wins.

### GraphQL

```
POST /app/v1/{slug}/graphql   {"query", "variables"?, "operationName"?}
```

The schema has only the enabled operations, and its input types list only the API's writable
columns. Exactly one mutation field is allowed per request.

```graphql
insertRow(data: RowInsert!, idempotencyKey: String): RowWrite!
updateRow(key: RowKey!, set: RowPatch!, expectedVersion: String, idempotencyKey: String): RowWrite!
deleteRow(key: RowKey!, expectedVersion: String, idempotencyKey: String): RowWrite!
type RowWrite { row: Row!  version: String! }
```

A refusal is `{"errors": [{"message", "extensions": {"code", "fields"?, "constraint"?}}]}`. The HTTP
status is `200`, except `401`, `403`, `404`, `405`, `413` and `429`, which keep theirs.

### Errors (REST: `{"error": {"code", "message", "fields"?, "constraint"?}}`)

| Status | `code` | When |
|---|---|---|
| 400 | `invalid_request` | Malformed body, unknown parameter, bad key |
| 401 | `unauthenticated` | No token, or not one core's workload issuer signed |
| 403 | `not_an_app` | A valid workload token that isn't a booth-streamlit app's |
| 403 | `operation_not_allowed` | Writes off, or the operation isn't in both the API's settings and the grant |
| 403 | `not_granted` | No live grant for this app on this API |
| 403 | `no_writable_columns` | Nothing is left writable (an owner must re-save) |
| 403 | `granting_owner_no_access` | The granting owner lost editor/owner, or hasn't signed in for 7 days |
| 404 | `not_found` | No such API in the app's workspace, or no row with that key |
| 405 | `invalid_request` | A read, or the wrong method |
| 409 | `conflict` | Unique or primary-key violation (`constraint` names it; never values) |
| 409 | `definition_stale` | The table changed under the API's snapshot; an editor must regenerate |
| 409 | `idempotency_in_progress` | The same key is still running |
| 409 | `idempotency_outcome_unknown` | A request with this key was interrupted; it may or may not have been applied |
| 412 | `version_mismatch` | `If-Match` / `expectedVersion` is stale |
| 413 | `invalid_request` | Body over 64 KiB |
| 422 | `unknown_column`, `column_not_writable`, `required`, `invalid_value` | Validation; `fields` lists each problem by field |
| 422 | `constraint` | A foreign-key, check or not-null constraint refused the row |
| 422 | `idempotency_mismatch` | The key was used with a different request |
| 429 | `rate_limited` | Over 10 writes/s (burst 50) for this app; `Retry-After` is set |
| 503 | `unavailable` | No write lease right now, the 5-second statement timeout, or booth-api isn't configured for app writes |

**Idempotency.**
- **Key space:** one per (API, app, key).
- **Lifetime:** a key is remembered for 24 hours.
- **Replay:** a replay returns the first response, with header `Idempotent-Replayed: true`.
- **Refused writes:** a refused or rolled-back write releases its key, so a corrected retry can reuse
  it.
- **Interrupted commits:** if the connection fails at COMMIT, the key stays pending, and later uses
  get `409 idempotency_outcome_unknown` rather than risk a duplicate row.

## What each layer enforces

| Layer | Refuses |
|---|---|
| booth-streamlit | Targets the app hasn't declared (its own copy, for the header and early refusal) |
| booth-api | No live grant; an operation or column outside the settings narrowed by the grant; an invalid row; over the rate |
| booth-core | Minting the `apiwrite:<api-id>` token if the granting owner lost access or hasn't signed in for 7 days; any non-`tablewrite` request from that token |
| booth-database | Anything outside the per-grant write group: other tables, other columns, undeclared operations, DDL, ignored or system tables |

## Limits and audit

- One row per request, 64 KiB, and a 5-second statement timeout.
- 10 writes per second per app, with a burst of 50. This is per booth-api pod (`writes.ratePerSecond`,
  `writes.burst`).
- The write audit log is kept 90 days (`writes.auditRetention`). Row values are never logged,
  neither in the audit log nor in booth-api's own log lines.
- `X-Booth-App-User` is recorded as reported by booth-streamlit's gate. It is never used for
  authorization. Recording who entered a row (`entered_by`) is the app's job (ADR 0117 point 3).
