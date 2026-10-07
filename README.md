# booth-api

Project Booth's API module (Manage nav group): turns a catalog dataset into a read-only REST and
GraphQL API, reachable by anyone holding an API key this module issues.

Scope for v0 is fixed by ADR 0100 in `booth-architecture`: REST (list/get, filtering, pagination,
an OpenAPI document) and GraphQL (read queries, schema generated from the dataset's columns), both
read-only; API keys created in this module's UI, shown once, stored only as a hash, scoped to
datasets, revocable; no rate limiting yet, but a maximum page size and a GraphQL depth/complexity
limit. Data comes from Postgres tables in `booth-database`, read through booth-core's credential
sidecar (ADR 0095); Iceberg tables and files are out of v0 (`ARCHITECTURE.md` item 53).

## Status: scaffold

What exists:

- `cmd/api`, `internal/server`: the service, with `/livez` (process only) and `/healthz`
  (readiness, pings the module's own database; the manifest's `healthCheckPath`).
- `charts/booth-api`: Deployment, Service, ServiceAccount (no token mounted) and the
  `BoothModule` registration (ADR 0019): `id: api`, native UI under Manage at `/apis`,
  `database: {enabled: true}`.
- `web/`: `@projectbooth/api-ui`, the native view booth-design mounts (ADR 0030/0031/0033). It
  renders a "not available yet" notice and makes no requests.
- CI per `contracts/testing-strategy.md` (below).

### Not built yet

Held until the design questions in `docs/` are answered, per the kickoff instructions:

- The API-key path (issue, hash, scope, revoke, verify). How an API-key caller with no OIDC
  session reaches a generated endpoint through booth-core's gateway needs a contract note first;
  today the gateway refuses any request without a verified token.
- The data-access layer (finding a dataset's database and table, sidecar scope, reading rows).
- REST/GraphQL generation, the management API (`/api/*`), and the real UI.

## Judgment calls in the scaffold

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
| `internal/server` | HTTP router |
| `charts/booth-api` | Helm chart, including the `BoothModule` CR |
| `test/contract` | Manifest/chart contract tests (`helm template`, no cluster) |
| `test/integration/fixtures` | booth-core's real `BoothModule` CRD, vendored from `booth-core/charts/booth-core/crds` |
| `hack/kind-integration.sh` | Layer-3 test on a kind cluster |
| `web` | `@projectbooth/api-ui` |
| `docs` | Design notes |

## Testing (ADR 0024)

| Layer | Where | When |
|---|---|---|
| Unit + contract | `.github/workflows/ci.yml`: `go` (gofmt, tidy, vet, `go test -race`, contract tests rendering the chart with helm), `web` (typecheck, lint, vitest, build), `helm-lint`, `image` (Docker build, not pushed) | Every push and PR; required by branch protection on `main` |
| Real cluster | `.github/workflows/integration.yml` runs `hack/kind-integration.sh` | Merge to `main`, nightly, manual |
| Cross-repo e2e | `booth-e2e` | Owned there |

The kind test installs the chart against booth-core's real CRD schema and a real PostgreSQL
standing in for core's ADR 0053 provisioning (a `postgres:16-alpine` pod and a
`booth-database-credentials` Secret in core's documented shape). It checks the `BoothModule` is
accepted, the pod becomes ready against the database, goes unready without restarting when the
database disappears, and recovers. It does not run a real booth-core yet; that comes in once the
module verifies tokens or calls the broker.

Local runs:

```sh
BOOTH_TEST_REQUIRE_HELM=1 go test -race ./...
(cd web && npm ci && npm run typecheck && npm run lint && npm test -- --run && npm run build)
bash hack/kind-integration.sh   # needs docker, kind, kubectl, helm
```

## UI package

Published to GitHub Packages as `@projectbooth/api-ui` by `.github/workflows/publish.yml` on an
`api-ui-v<version>` tag matching `web/package.json`. Nothing is published at scaffold stage, and
booth-design does not mount it yet. Per ADR 0097 the shell scans `dist/*.js` for Tailwind class
names, so class names must stay complete static strings.
