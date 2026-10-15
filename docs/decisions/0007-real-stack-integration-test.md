# 0007: The real-stack integration test

Date: 2026-10-08. `hack/realstack-integration.sh`, run by the `realstack` job in
`.github/workflows/integration.yml` (merge to main, nightly, manual).

## What runs

A kind cluster with Keycloak (dev mode, a test realm: alice and erin own `acme`, bob and dave edit
it, carol owns `globex`) and **real** booth-core, booth-database and booth-catalog, each built from
source at a pinned commit and installed from its own chart with the values booth-e2e uses, plus
this booth-api with data access and app writes on (`oidc.workloadIssuerUrl` = core). Two stand-ins:
- **booth-catalog's own metadata database.** Its chart takes a DSN Secret rather than declaring
  `database`, so the script gives it a small Postgres, as booth-e2e does with a shim.
- **A `streamlit` BoothModule with `workloadIdentity.mint` and nothing behind it.** Core delivers its
  minting credential, and the check mints real app tokens (`streamlit:acme:<app>`, roleCeiling
  viewer) with it, exactly as booth-streamlit does.

`test/integration/realstack/check` runs as a Job inside the cluster and only talks to Keycloak and
booth-core (its API, the gateway and the public route):

1. Sign in five people; core records their roles.
2. Create tables in `acme`'s and `globex`'s booth-database databases with a readwrite credential
   from core's broker.
3. Register them in booth-catalog as `format: "postgres"` datasets (ADR 0102).
4. Generate APIs through the gateway. This is booth-api fetching the dataset from the catalog as
   the caller, minting a workload token, starting the real credential sidecar, and introspecting the
   real table.
5. Issue keys and use them on `/modules/api/public/v1/...`:
   - REST read and filter, exact bigint/numeric, GraphQL, the OpenAPI document;
   - 403 outside the key's scope; 401 with no key, an unknown key, or a platform JWT;
   - 404 across workspaces, including with a forged `X-Booth-Workspace`;
   - core's 404 for a `..` traversal out of `/v1/`.
6. Revoke a key: the next request is 401, and other keys are unaffected.
7. Remove bob from `acme` in Keycloak and have him sign in again: his key (never used before, so
   no sidecar is running for him) is refused with 403 at minting, while dave's key keeps working.
8. Create `nfl_data.games` in `acme`'s database and register nothing: booth-database's
   `database.table.*` event makes booth-catalog list it (`source: "auto"`, ADR 0116). Generate an API
   from that dataset, and read it with a key over REST and GraphQL. booth-database publishes every
   10 s here and ignores `public.*` on top of its defaults, so step 3's hand-registered tables can't
   race an auto-listing.
9. App writes (ADR 0117, `0008`), on an auto-listed `finance_input.entries`:
   - **Owners only.** An editor can't turn writes on. alice turns on insert and update for three
     columns, which sends the full scope through core's broker to booth-database.
   - **The app writes.**
     - It gets 403 before a grant.
     - After alice grants it, it inserts, updates with `If-Match` (412 when stale), and inserts
       over GraphQL, where the same idempotency key replays.
     - A non-writable column is 422; delete is 403.
   - **App tokens only write.**
     - An app token is 405 on a read, and 401 on the management API and on the public route.
     - A read key can read but can't write.
     - Another workspace's app gets 404.
     - Forged `X-Booth-*` headers change nothing.
   - **The lease booth-database issues for the same scope** inserts into the table but can't touch
     another table, another column, delete, or run DDL.
   - **The granting owner loses access.** erin grants a second app, which writes. erin is then
     removed from `acme` and signs in again. alice re-saves the settings, which replaces the write
     sidecars. The second app's next write fails at mint (403 `granting_owner_no_access`), while
     alice's grant keeps working.
   - **Revocation.**
     - Revoking a grant stops the app.
     - revoke-principal revokes the other app's grant.
     - The write log has entries and no row values.
     - Turning writes off revokes the database grant: the lease from the step above no longer
       connects.
   - **Afterwards, the script checks booth-api's log:** a write sidecar started, one stopped on
     revocation and on writes off, and no row value appears.

## What it doesn't cover

- A key whose creator loses access *while* a sidecar is already running for them stops at the next
  renewal (within about 6.7 minutes for core's 10-minute tokens). That path is covered by
  `internal/sidecars` unit tests with a fake sidecar, not here, to keep the run short.
- Idle shutdown and lease renewal of the real sidecar: renewal happens at half of booth-database's
  one-hour lease, longer than the run.
- The 7-day recency window itself (core's, `BOOTH_WORKLOAD_OWNER_MAX_AGE`). The test removes
  membership instead, which goes through the same refusal.

## Pins

They are bumped deliberately, the same practice as booth-logging's `real-core` job.
- **`CORE_REF`:** v0.3.5, `a562691`. It has the `tablewrite` broker access, `apiwrite:` reserved to
  module `api`, and the published credential-sidecar booth-api's Dockerfile pins.
- **`DATABASE_REF`:** `4c730d3`, booth-database's `main` at PR #15's merge (table-scoped write
  leases). It is **not a release tag yet**; move it to the tag when one is cut.
- **`CATALOG_REF`:** v0.1.5, `c898575`.

Core 0.3.x bundles Keycloak and an Ingress by default; the script turns both off (it brings its own
Keycloak and talks to core's Service).
