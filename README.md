# booth-api

Project Booth's API module (Manage nav group): turns a catalog dataset into a read-only REST and
GraphQL API, reachable by anyone holding an API key this module issues.

Scope for v0 is fixed by ADR 0100 in `booth-architecture`: REST (list/get, filtering, pagination,
an OpenAPI document) and GraphQL (read queries, schema generated from the dataset's columns), both
read-only; API keys created in this module's UI, shown once, stored only as a hash, scoped to
datasets, revocable; no rate limiting yet, but a maximum page size and a GraphQL depth/complexity
limit. Data comes from Postgres tables in `booth-database`, read through booth-core's credential
sidecar (ADR 0095); Iceberg tables and files are out of v0 (`ARCHITECTURE.md` item 53).

## Using a generated API

This describes what is built today. Contracts: ADR 0100 (scope), ADR 0101 (public route), ADR 0102
(postgres datasets), ADR 0103 (data access).

### 1. Register the table in the catalog

A generated API reads one table in your workspace's booth-database database. Register that table in
**Catalog** as a dataset with `format: "postgres"` and `postgresTable: {schema, name}` (ADR 0102).
If you give the dataset a column list, it must name exactly the table's columns. Leave it empty if
you don't want to keep one; booth-api reads the real columns from the table either way.

### 2. Generate the API (editor or owner)

In **Manage → API** (`/apis`), pick the dataset under "Generate an API from a dataset". booth-api
reads the table's columns and stores them as the API's schema; it keeps that schema until you press
**Regenerate**. Each API card shows:

- its **slug**, which is part of every URL below (from the dataset name, e.g. `orders`);
- its columns, with "Show GraphQL schema" and "Show OpenAPI document";
- its public URLs.

Columns whose type has no faithful JSON/GraphQL form (`bytea`, intervals, etc.) are left out and
listed under the schema.

### 3. Create a key (editor or owner)

Under **API keys**, give the key a name, tick the datasets it may read, and press **Create key**.

- The key, `booth_ak_<id>_<secret>`, is shown **once**. Copy it then. booth-api keeps only a hash.
- It reads only the datasets you ticked, in this workspace.
- It reads data **as you**. It stops working if you lose access to the workspace, or if you don't
  sign in to Booth for 7 days (booth-core's default). Within about 7 minutes of that, requests
  answer 403. Create keys for long-running jobs as someone who signs in regularly.
- **Revoke** takes effect on the key's next request (401). The list shows when each key was last
  used.

### Without the UI

Steps 1 to 3 are ordinary HTTP calls through booth-core's gateway, made with a person's Booth
token (`Authorization: Bearer <token>`) and the workspace (`X-Workspace: <slug>`). They're what the
Catalog and API pages send, and they're useful for scripting a setup:

```sh
H=(-H "Authorization: Bearer $TOKEN" -H "X-Workspace: acme" -H "Content-Type: application/json")

# 1. Register the table (returns the dataset, with its "id")
curl "${H[@]}" "<booth URL>/modules/catalog/api/datasets" -d '{"name": "Orders", "description": "",
  "format": "postgres", "postgresTable": {"schema": "public", "name": "orders"}, "schema": [], "tags": []}'

# 2. Generate its API (returns the API, with its "slug")
curl "${H[@]}" "<booth URL>/modules/api/api/apis" -d '{"datasetId": "<dataset id>"}'

# 3. Create a key (the response's "secret" is the key; it is not shown again)
curl "${H[@]}" "<booth URL>/modules/api/api/keys" -d '{"name": "nightly export", "datasetIds": ["<dataset id>"]}'

# List keys, and revoke one
curl "${H[@]}" "<booth URL>/modules/api/api/keys"
curl "${H[@]}" -X POST "<booth URL>/modules/api/api/keys/<key id>/revoke"
```

### 4. Call it

Every request goes through booth-core's gateway at

```
<booth URL>/modules/api/public/v1/<slug>/...
Authorization: Bearer booth_ak_<id>_<secret>
```

No Booth login is involved, only the key. In the examples, `BASE` is
`https://booth.example/modules/api/public/v1/orders` and `KEY` is your key.

**REST**

With curl, use `-g` (`--globoff`) on any URL containing `filter[...]`: otherwise curl reads the
brackets as one of its own URL patterns and fails with `curl: (3) bad range`.

```sh
# A page of rows (default 50, at most 500 per page)
curl -H "Authorization: Bearer $KEY" "$BASE/rows"

# Filter, choose fields, order (a leading '-' is descending)
curl -g -H "Authorization: Bearer $KEY" \
  "$BASE/rows?filter[region]=emea&filter[amount][gte]=10&fields=id,amount&order=-placed&limit=100"

# Next page: pass page.nextCursor back as `after`, with the same filters and order, until it is null
curl -g -H "Authorization: Bearer $KEY" "$BASE/rows?filter[region]=emea&order=-placed&after=<nextCursor>"

# One row by primary key (tables with a single-column primary key)
curl -H "Authorization: Bearer $KEY" "$BASE/rows/42"

# The OpenAPI 3.1 document for this API
curl -H "Authorization: Bearer $KEY" "$BASE/openapi.json"
```

A list answers `{"data": [ ...rows... ], "page": {"hasNext": true, "nextCursor": "..."}}`, and a
single row answers `{"data": {...}}`.

Filters are written `filter[field][op]=value`, and `filter[field]=value` means `eq`. Filters are
combined with AND.

| Operator | For |
|---|---|
| `eq`, `neq`, `isNull` (`true`/`false`) | every filterable column |
| `in` (repeat the parameter: `filter[f][in]=a&filter[f][in]=b`) | every filterable column |
| `lt`, `lte`, `gt`, `gte` | numbers, text, dates and times |
| `startsWith` (literal prefix; `%` and `_` are not wildcards) | text |

`order` (and GraphQL's `orderBy`) takes columns declared `NOT NULL`, since paging compares cursor
values and NULLs break that; the primary key is always added as a tiebreak. Array and JSON columns
can be returned but not filtered. Any parameter not listed here is a 400, so
a typo can't quietly return more than you asked for.

**GraphQL**

```sh
curl -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" "$BASE/graphql" -d '{
  "query": "query($r: String) { rows(where: {region: {eq: $r}}, orderBy: [{field: placed, direction: DESC}], first: 20) { nodes { id amount } pageInfo { hasNextPage endCursor } } }",
  "variables": {"r": "emea"}
}'
```

- `rows(where, orderBy, first, after)` returns `nodes` and `pageInfo`.
- `row(<primary key>: ...)` fetches one row (tables with a single-column key).
- `where` also takes `and`, `or` and `not`.
- GET with `?query=` works too.
- There are no mutations; the API is read-only.
- The full schema is under "Show GraphQL schema" in the UI, or by introspection.

**Values.** `bigint` and `numeric` come back as strings (`"9007199254740993"`, `"12.50"`), so no
precision is lost. Dates and times are ISO 8601 strings. JSON columns are JSON. Filter values use
the same formats: `2026-10-08T09:00:00Z`, not `yesterday`.

**Limits** (correctness limits; there is no rate limiting yet, ADR 0100):

- page size at most 500, and asking for more is an error, not a silent clamp;
- `in` lists at most 100 values; at most 50 comparisons per filter;
- GraphQL: nesting depth 8, cost 10,000 (`first` × selected fields, summed), 5 root fields,
  `and`/`or`/`not` nested at most 4 deep, query at most 16 KiB, request body at most 64 KiB;
- every query is read-only and cancelled after 5 seconds.

**Status codes**

| Code | Means |
|---|---|
| 200 | OK (GraphQL query errors also come back as 200, in `errors`) |
| 400 | Something about the request: an unknown parameter, a bad value, over a limit |
| 401 | No key, or the key is unknown, wrong or revoked |
| 403 | The key isn't scoped to this API's dataset, or its creator no longer has access |
| 404 | No API with that slug in the key's workspace, or no row with that key |
| 405 | Anything but GET/HEAD (REST) or GET/POST (GraphQL) |
| 413 | A GraphQL request body over 64 KiB |
| 503 | The query took longer than 5 seconds, or reading workspace data isn't configured |

REST errors are `{"error": {"message": "..."}}` and GraphQL errors `{"errors": [{"message": "..."}]}`.

**Known limitation: primary keys containing `/`.** booth-core decodes and cleans every public path
before forwarding it (its traversal fix), so `GET .../rows/a%2Fb` reaches booth-api as `/rows/a/b`
and can't match a key containing `/`. Fetch such a row with a filter instead:
`GET .../rows?filter[<key column>]=a%2Fb`.

**Don't expose this to the internet yet.** The public route has no rate limiting (ADR 0100, ADR 0101),
so keep Booth on a network you control until that is decided.

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
  dataset scope, returning its own 401/403/404. `X-Booth-*` headers are never read there. A REST
  row whose primary key contains `/` is reachable only through `filter[...]` (see "Known
  limitation" above).

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
