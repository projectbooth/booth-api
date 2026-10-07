# booth-api v0 design note

Date: 2026-10-07. Status: **proposal, waiting on answers to §1 and §2.** Nothing in §1 or §2 is
built. §3 can start once §2 is settled, since it needs a real table to read.

Scope is ADR 0100 (REST and GraphQL, read-only, module-issued API keys, page-size and GraphQL
depth/complexity limits, no rate limiting) with the v0 data source in `ARCHITECTURE.md` item 53
(Postgres tables from `booth-database`, read through the ADR 0095 credential sidecar).

Everything stated as current behaviour below was checked in the sibling repos' code on 2026-10-07,
with the file named. It was not taken from the briefs.

---

## 1. How an API-key caller reaches a generated endpoint through core's gateway

### What happens today

A caller with an API key and no OIDC session cannot reach booth-api through core at all:

- `booth-core/internal/api/server.go`: `/modules/{id}/*` is mounted inside a group using
  `auth.Middleware` (with the workload-token verifier added, ADR 0059). A request without a token
  core can verify gets 401 before the gateway runs. `X-Workspace` is also required there
  (ADR 0025).
- `booth-core/internal/gateway/proxy.go`: the gateway **overwrites** `Authorization` with the
  verified bearer token and sets `X-Booth-Workspace`/`X-Booth-Role`. So an API key sent in
  `Authorization` would not reach the module even if the request got past the middleware.
- `booth-design/docker/nginx.conf.template` proxies `/api/` and `/modules/` on the shell's origin
  to core. That is the only path in from outside the cluster today (`ARCHITECTURE.md` item 37a: no
  Ingress, TLS or public-hostname story yet).

ADR 0100 says core's gateway routes to the generated endpoints and booth-api decides whether a key
is valid, and that how the gateway exposes such a path without an OIDC session needs its own note
in `contracts/` before it ships. This section sets out the options for that note. It is not the
note.

### Options

**A. An opt-in, unauthenticated pass-through route in core's gateway (recommended).**
A module declares on its manifest that some of its paths are public, and core proxies those paths
with no OIDC check, leaving authentication to the module. Sketch only; core and the coordinator
own the real shape:

```yaml
# BoothModule spec (new optional field, contracts/module-manifest.md)
publicRoutes:
  pathPrefixes: ["/v1/"]        # module-relative; core serves them at /modules/api/public/v1/...
```

What booth-api would need core to guarantee on that route:

1. Only modules declaring the field get it, and only for the declared prefixes. Everything else
   about `/modules/{id}/*` stays as it is.
2. No `auth.Middleware`, no `X-Workspace` requirement, nothing written to the user directory
   (ADR 0047). The workspace comes from the key, which the module resolves.
3. `Authorization` is passed through unchanged (booth-api keys would be sent as
   `Authorization: Bearer booth_ak_...`, the prefix telling them apart from a JWT). Or, if core
   prefers not to forward `Authorization` on an unauthenticated route, a fixed `X-Booth-Api-Key`
   header. Either works; booth-api just needs to know which.
4. Core **strips** any inbound `X-Booth-Workspace`, `X-Booth-Role` and `X-Booth-Identity` on this
   route, so nothing downstream can mistake a caller-supplied header for one core vouched for.
   booth-api will not read them on public paths regardless (defence in depth, ADR 0041).
5. A distinct URL prefix (the `/public/` segment above, or a separate top-level prefix) so that
   logs, NetworkPolicy and any later rate limiting can tell public traffic from authenticated
   traffic without parsing manifests.
6. Request size and timeout bounds like the existing proxy's.

Consequences: a small contract change (`module-manifest.md` and the CRD, `core-platform-api.md`'s
gateway section) and gateway work in booth-core. booth-design's nginx already forwards `/modules/`,
so a prefix under it needs no shell change; a new top-level prefix would. It is generic: any
future module that serves a non-person caller (webhooks, for instance) would use the same field.
The risk is that it is the first route in core that forwards traffic nobody has authenticated, and
ADR 0100's "no rate limiting yet" means core has nothing in front of it either. ADR 0100 already
says to revisit rate limiting before generated endpoints are exposed beyond the user's own network,
and that applies here.

**B. Expose booth-api's own Service directly (separate Service/Ingress), not through core.**
No core change at all. But it contradicts ADR 0100's "core's gateway routes to the generated
endpoints" and ADR 0007's single-gateway shape, and it depends on the Ingress/TLS story that
`ARCHITECTURE.md` item 37a says does not exist. Not recommended for v0 unless the coordinator wants
to reopen ADR 0100 on this point.

**C. Exchange the key for a token core already trusts.**
booth-api would mint a workload token (ADR 0056) for a key and the caller would present that to
the gateway. The exchange endpoint itself has to be reachable without an OIDC session, so this
still needs A for one path, and it hands a platform token to an external client, which ADR 0056
never intended. Not recommended.

**D. Core verifies API keys.** Rejected by ADR 0100. Listed only for completeness.

### What I need from core / the coordinator

- A decision between A and B (A recommended), recorded as the contract note ADR 0100 asks for.
- If A: the manifest field name and shape, the public URL prefix, and which header carries the key
  (`Authorization` passed through, or a dedicated header). booth-api builds against whatever is
  settled; it does not need these to be the names sketched above.
- Confirmation that the module, not core, returns 401/403 on public paths, so the error format is
  booth-api's own.

---

## 2. Finding a dataset's database and table, and getting sidecar scope for it

This splits into three separate problems. The first two need decisions outside this repo.

### 2a. The catalog cannot say "this dataset is a booth-database table"

`booth-catalog/internal/data/model.go`: a `Dataset` has `format: "file" | "iceberg"` (ADR 0085),
an Iceberg-only `table{namespace, name, uuid, currentSnapshotId}` block, and a **required**
`location: {backendId, path}` that must be a `booth-storage` reference (validated in
`internal/asset/asset.go`, `NormalizeLocation`). There is no Postgres format and no field that
names a database table, and a Postgres table cannot even be registered by hand without inventing a
storage location for it.

The brief says to stop here rather than guess, so these are options, not a choice:

- **2a-i. Extend the catalog the way ADR 0085 did for Iceberg (recommended).** Add
  `format: "postgres"` and a block such as `table: {schema, name}` (the workspace, and so the
  database, is already the dataset's own workspace), with `location` not required for that format.
  Who populates it is a second question: a user registering by hand through the catalog's existing
  write API (editor/owner, ADR 0048) is enough for v0; `booth-database` publishing `table.*`-style
  events later would mirror ADR 0085/0086. This is an asset-model change, which `ARCHITECTURE.md`
  item 12 says no module may make on its own, so it needs a ruling and catalog work.
- **2a-ii. booth-api keeps the mapping itself.** The user picks a catalog dataset and then picks a
  table in the workspace database, and booth-api stores the pair. No catalog change, but the
  catalog entry then describes data it does not actually point at, and two modules would disagree
  about where a dataset lives. Not recommended.
- **2a-iii. booth-api lists the workspace database's tables directly and skips the catalog.**
  Contradicts the brief ("consume booth-catalog to know what's available to expose") and ADR 0100's
  "turn a catalog dataset into an API". Not recommended.

### 2b. Which database

Settled by `booth-database` itself, and needs nothing new: there is one database per workspace
(ADR 0081), and the `postgres` broker scope accepts only an optional `workspace`, which must equal
the request's own workspace (`booth-database/internal/credentialbroker/provider.go`, `pgScope`).
The database name in the lease is derived by `booth-database`; booth-api never needs to know it.
The sidecar puts it in the connection for us.

### 2c. Sidecar scope and identity in a multi-workspace pod

This is the part that does not fit the existing sidecar adopters, and I would like it confirmed
before building.

Facts:

- A `read` lease grants `SELECT` on **every** table in the workspace's `public` schema
  (`booth-database/internal/provision/provision.go`), not one table. The provider refuses any
  narrower scope by design ("refuse rather than widen"). So restricting a generated API to its one
  table is booth-api's job: it only ever queries the table named in the dataset, using identifiers
  from introspection, never from the request.
- The sidecar authenticates to the broker as whatever identity the pod has (`--token` or
  `--token-file`, plus `--workspace`; `booth-core/cmd/credential-sidecar/main.go`). Notebook and
  pipeline pods have exactly one person or run per pod. booth-api's pod serves every workspace and
  its callers are API keys, not people, so there is no identity in the pod to hand the sidecar.
- `contracts/credential-sidecar.md` "Deployment granularity" already documents the matching shape:
  a shared pod with several live identities runs **one sidecar process per identity**, with
  `--token-file` and a Unix-socket `--listen`.

Proposed (needs a yes or a correction):

1. **Identity: a workload token per workspace, minted by booth-api** (manifest
   `workloadIdentity: {mint: true}`, ADR 0056/0058), with `subject: "apikeys:<workspace>"`,
   `roleCeiling: viewer` (a `read` lease needs no more, `contracts/credential-broker.md`), and
   `owner` set to a person in that workspace (see the open point below). booth-api keeps each
   workspace's token current in a file.
2. **One sidecar process per workspace with live generated APIs**, started on first use and
   stopped after an idle period: `--kind=postgres --access=read --workspace=<ws>
   --token-file=<per-ws file> --listen=unix:///<private dir>/<ws>.sock`. booth-api opens a small
   pool per workspace against that socket (`DATABASE_URL`-style, loopback only, as ADR 0095
   intends) and never sees a database password. The binary is copied into booth-api's image from
   the digest-pinned `ghcr.io/projectbooth/credential-sidecar` image, the way `booth-pipeline`
   took the binary earlier.
3. **Connection lifetime**: pools use liveness checks and recycle connections well inside the
   sidecar's guaranteed minimum (about half a lease), per the contract's "Connection lifetime"
   section. A query failing on a dropped connection is retried once on a fresh one, since every
   query here is a read.
4. **Network**: booth-api declares `database`, so core labels its namespace
   `booth.projectbooth.io/database-client=true`, which `booth-database`'s NetworkPolicy already
   admits. booth-api's chart would gain the same opt-in `boothDatabase.url` value the notebook and
   pipeline charts use (ADR 0092), gating the sidecar binary's use.

Open points in 2c that need the coordinator:

- **Whose `owner` the workload token names.** Minting refuses once the named owner has not signed
  in for 7 days (`BOOTH_WORKLOAD_OWNER_MAX_AGE`, ADR 0058). API keys are meant for scripts that
  run long after anyone logs in, so using the key's creator would make an API go dark a week after
  they stop logging in. ADR 0088 says `owner` may be any current workspace member, and suggested a
  "fallback current owner" for `booth-lakehouse`. booth-api has no way to list a workspace's
  current owners today (the user directory is lookup/search only, ADR 0047/0052). Options: accept
  the same 7-day limit `booth-lakehouse` accepted (ADR 0084), record the creator plus a fallback
  chosen in the UI, or a non-person service identity, which ADR 0088 deferred until a real need
  appeared. This may be that need.
- **Whether a multi-workspace module minting workload tokens for itself is acceptable.** Today's
  minters (`booth-pipeline`, `booth-notebooks`) mint on behalf of a person's run. booth-api would
  hold read access to every workspace with a live API, which is what a multi-tenant data API
  needs, but it is a wider standing position than any module has had so far.
- **Alternative to the sidecar**: booth-api calls `POST /api/credentials` itself and manages leases
  in Go. That is simpler inside one process, but it is the per-module renewal logic ADR 0095 was
  written to avoid, and you asked for the sidecar, so it is not proposed.

---

## 3. Generating the GraphQL schema from dataset columns

This depends on §2 for a real table, but nothing in it needs a contract change.

**Source of truth for columns.** The catalog's `schema` is free-form (`"bigint"`,
`"timestamp(6)"`; `booth-catalog/internal/data/model.go` says it deliberately doesn't normalize
types), so it cannot drive types reliably. At generation time booth-api introspects the real table
(`information_schema.columns`, plus the primary key from `pg_index`) through the sidecar
connection, uses Postgres's types, and takes descriptions from the catalog where names match. A
catalog column missing from the table, or the reverse, is shown to the user at generation time and
blocks generation until the catalog entry is corrected, so the API never claims columns the
catalog doesn't.

**Snapshot, not live.** The generated definition (columns, types, primary key, exposed name) is
stored in booth-api's own database when the user generates the API. Both REST and GraphQL are
built from that stored definition, so the OpenAPI document and GraphQL schema don't change under a
client's feet. Each query checks the stored columns still exist; if the table has changed, the
endpoint returns a clear error and the UI offers "regenerate".

**Type mapping (Postgres to GraphQL; REST/JSON follows the same rules):**

| Postgres | GraphQL | Note |
|---|---|---|
| `smallint`, `integer` | `Int` | |
| `bigint` | `BigInt` (custom scalar, serialized as a string) | GraphQL `Int` is 32-bit; JSON numbers lose precision past 2^53 |
| `numeric`/`decimal` | `Decimal` (string) | exact value preserved |
| `real`, `double precision` | `Float` | |
| `boolean` | `Boolean` | |
| `text`, `varchar`, `char`, `citext` | `String` | |
| `uuid` | `ID` | |
| `date`, `timestamp`, `timestamptz`, `time` | `Date`/`DateTime`/`Time` (ISO 8601 strings) | |
| `json`, `jsonb` | `JSON` (custom scalar) | not filterable in v0 |
| arrays of the above | `[T]` | not filterable in v0 |
| `bytea`, and anything else | omitted, and listed in the UI as not exposed | no silent lossy conversion |

**Names.** GraphQL names must match `[_A-Za-z][_0-9A-Za-z]*` and must not start with `__`. A
column name that doesn't fit is converted (invalid characters to `_`, a leading digit prefixed with
`_`), collisions after conversion are numbered, and the mapping is stored with the definition. SQL
always uses the real column name, quoted, from the stored definition, never from request input.

**Schema shape, one GraphQL endpoint per generated API** (so a key's dataset scope maps straight
onto endpoints):

```graphql
type Query {
  rows(where: RowFilter, orderBy: [RowOrder!], first: Int = 50, after: String): RowConnection!
  row(id: ID!): Row            # only when the table has a single-column primary key
}
type Row { ...one field per exposed column... }
type RowConnection { nodes: [Row!]!, pageInfo: PageInfo! }
input RowFilter { and: [RowFilter!], or: [RowFilter!], <column>: <Type>Filter }
input IntFilter { eq: Int, neq: Int, lt: Int, lte: Int, gt: Int, gte: Int, in: [Int!], isNull: Boolean }
# ...one filter input per scalar type; String adds `like` (anchored prefix match only in v0)
```

No mutations (ADR 0100). Cursor pagination uses keyset order on the primary key (or `ctid` order
with offset fallback for tables without one, flagged in the UI as slower and not stable under
concurrent writes).

**Limits (ADR 0100 requires them even without rate limiting):**

- Page size: `first` defaults to 50 and is capped at a chart-configured maximum (default 500); a
  larger value is an error, not a silent clamp, so a client knows it got less than it asked for.
  REST `limit` uses the same cap.
- Depth: the schema has no relations, so a legitimate query is at most about four levels deep
  (`rows > nodes > field`, `pageInfo`); queries deeper than a fixed limit (8) are rejected before
  execution. This mainly stops abuse of introspection and aliases.
- Complexity: cost is `first` multiplied by the number of selected fields, summed over aliases,
  with a cap (default 10,000) and a cap on the number of aliased root fields (default 5), checked on
  the parsed query before any SQL runs.
- Filters: nesting depth of `and`/`or` capped (default 4) and `in` lists capped (default 100).
- Every query runs with a statement timeout (default 5s) on a read-only lease.

**Library.** gqlgen generates Go code at build time, so it cannot serve schemas created at
runtime. Candidates are `graphql-go/graphql` (builds a schema at runtime and executes it), or
`vektah/gqlparser` for parsing and validation with booth-api's own small resolver over the parsed
query. Either way the depth and complexity checks are booth-api's own walk over the parsed
document, since neither library enforces them. I'll compare the two on a spike before choosing,
and record the choice in `docs/`.

---

## Summary of what I'm asking for

1. **§1**: choose option A or B. If A, the contract note with the field name, URL prefix and key
   header.
2. **§2a**: how the catalog records a Postgres table (2a-i recommended; needs catalog work and a
   ruling under item 12).
3. **§2c**: confirm per-workspace workload tokens plus one sidecar process per workspace, and rule
   on whose `owner` the token names.

Until those come back I won't build the key auth path or the data-access layer. §3 can be built
against a fixture table once §2a is settled.
