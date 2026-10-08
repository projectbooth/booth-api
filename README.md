# booth-api

Project Booth's API module (Manage nav group): turns a catalog dataset into a read-only REST and
GraphQL API, reachable by anyone holding an API key this module issues.

Scope for v0 is fixed by ADR 0100 in `booth-architecture`: REST (list/get, filtering, pagination,
an OpenAPI document) and GraphQL (read queries, schema generated from the dataset's columns), both
read-only; API keys created in this module's UI, shown once, stored only as a hash, scoped to
datasets, revocable; no rate limiting yet, but a maximum page size and a GraphQL depth/complexity
limit. Data comes from Postgres tables in `booth-database`, read through booth-core's credential
sidecar (ADR 0095); Iceberg tables and files are out of v0 (`ARCHITECTURE.md` item 53).

## Status

Built (details and the choices made: `docs/decisions/0001-keys-and-api-definitions.md`):

- The service (`cmd/api`), with `/livez` (process only) and `/healthz` (readiness: the module's own
  database is reachable and its migrations have applied; the manifest's `healthCheckPath`).
- The management API at `/api/*`, behind OIDC verification (ADR 0041): generate an API from a
  catalog dataset (a snapshot of its table), regenerate, delete; issue API keys (shown once,
  stored as a hash, scoped to datasets), list, revoke.
- The GraphQL engine (`internal/gql`; library choice `docs/decisions/0002`, schema and paging
  `docs/decisions/0003`): schema generated from a definition's snapshot, query validation, every
  ADR 0100 limit checked before SQL, one read-only parameterized statement per root field, keyset
  pagination, introspection.
- The REST endpoint (`internal/rest`, `docs/decisions/0004`): `GET /rows` with filters, ordering,
  field selection and cursors, `GET /rows/{key}`, and a generated OpenAPI 3.1 document linted in CI.
  It shares the query layer (`internal/table`) with GraphQL, so both protocols compile the same
  filter to the same SQL and their cursors are interchangeable.
- The public, key-authenticated path (`internal/public`, `docs/decisions/0005`): core's gateway
  serves `/modules/api/public/v1/<slug>/...` with no platform login (ADR 0101, manifest
  `publicRoutes: [/v1/]`), and booth-api checks the API key, the API's workspace and the key's
  dataset scope, returning its own 401/403/404. `X-Booth-*` headers are never read there.

- Reading workspace data (`internal/sidecars`, `internal/workload`, `docs/decisions/0006`): a
  workload token per workspace and key creator (ADR 0103), one booth-core credential-sidecar
  process per such pair on a private Unix socket, pooled connections, renewal, idle shutdown. On
  when `core.url` is set; without it data requests answer 503.

- A real-stack integration test (`hack/realstack-integration.sh`, `docs/decisions/0007`) against real
  Keycloak, booth-core, booth-database and booth-catalog on kind: keys, scope, workspace isolation,
  revocation and a creator losing access, all through core's gateway and public route.

## Judgment calls in the scaffold (see also `docs/decisions/`)

- **`navPath: /apis`, not `/api`.** The shell's nginx proxies `/api/` and `/modules/` to booth-core
  on the same origin, so a module page under `/api/...` would never reach the shell. The contract
  test refuses a `navPath` under `/api/`, `/modules/` or `/iframe/`.
- **`database: {enabled: true}`** from the start: API key hashes and generated-API definitions
  need persistent storage, and ADR 0053 is the platform's way to get it. This is the module's own
  database on core's Postgres, not a workspace database in `booth-database`.
- **`/healthz` depends on that database**, since nothing the module serves can work without its
  keys; `/livez` does not, so an outage makes the pod unready rather than restarting it.

## Layout

| Path | What |
|---|---|
| `cmd/api` | Entrypoint |
| `internal/config` | Environment configuration, 1:1 with chart values |
| `internal/server` | HTTP router: probes, `/api/*` |
| `internal/auth` | OIDC verification and role derivation (ADR 0041) |
| `internal/api` | Management API handlers |
| `internal/apis` | Dataset-to-API definition flow |
| `internal/apidef` | The definition snapshot, reconciliation, slugs |
| `internal/table` | Query layer shared by both protocols: model, filters, paging, limits |
| `internal/gql` | GraphQL schema generation, limits, execution, introspection |
| `internal/rest` | REST handler and OpenAPI document |
| `internal/public` | Key-authenticated public path serving both protocols |
| `internal/workload` | Workload-token minting client (ADR 0103) |
| `internal/sidecars` | Per-workspace-and-creator credential sidecars and their pools |
| `internal/catalog` | booth-catalog client, through core's gateway as the caller |
| `internal/source` | Table introspection; per-workspace pools (not wired yet) |
| `internal/keys` | Key format, hashing, issue/revoke/verify |
| `internal/store` | Own-database persistence and migrations |
| `internal/db` | Pool and migration helpers; `dbtest` for tests on real Postgres |
| `charts/booth-api` | Helm chart, including the `BoothModule` CR |
| `test/contract` | Manifest/chart contract tests (`helm template`, no cluster) |
| `test/integration/fixtures` | booth-core's real `BoothModule` CRD, vendored from `booth-core/charts/booth-core/crds` |
| `hack/kind-integration.sh` | Layer-3 test on a kind cluster |
| `web` | `@projectbooth/api-ui` |
| `docs` | Design notes |

## Testing (ADR 0024)

| Layer | Where | When |
|---|---|---|
| Unit + contract | `.github/workflows/ci.yml`: `go` (gofmt, tidy, vet, `go test -race` against a real PostgreSQL from `hack/docker-compose.yml`, contract tests rendering the chart with helm, Redocly lint of the generated OpenAPI document), `web` (typecheck, lint, vitest, build), `helm-lint`, `image` (Docker build, not pushed) | Every push and PR; required by branch protection on `main` |
| Real cluster | `.github/workflows/integration.yml`: `kind` runs `hack/kind-integration.sh` (this chart alone); `realstack` runs `hack/realstack-integration.sh` (real core, booth-database, booth-catalog at pinned commits) | Merge to `main`, nightly, manual |
| Cross-repo e2e | `booth-e2e` | Owned there |

The kind test installs the chart against booth-core's real CRD schema and a real PostgreSQL
standing in for core's ADR 0053 provisioning (a `postgres:16-alpine` pod and a
`booth-database-credentials` Secret in core's documented shape). It checks the `BoothModule` is
accepted, migrations create the expected tables, the management API answers 503 with no OIDC
configured, and the pod becomes ready against the database, goes unready without restarting when
the database disappears, and recovers. It does not run a real booth-core yet; that comes in once the
module verifies tokens or calls the broker.

Local runs:

```sh
docker compose -f hack/docker-compose.yml up -d --wait && eval "$(sh hack/test-env.sh)"
BOOTH_TEST_REQUIRE_HELM=1 BOOTH_TEST_REQUIRE_POSTGRES=1 go test -race ./...
(cd web && npm ci && npm run typecheck && npm run lint && npm test -- --run && npm run build)
bash hack/kind-integration.sh   # needs docker, kind, kubectl, helm
```

## UI package

Published to GitHub Packages as `@projectbooth/api-ui` by `.github/workflows/publish.yml` on an
`api-ui-v<version>` tag matching `web/package.json`. Nothing is published yet, and booth-design
does not mount it yet. Per ADR 0097 the shell scans `dist/*.js` for Tailwind class
names, so class names must stay complete static strings.
