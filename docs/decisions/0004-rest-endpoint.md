# 0004: The REST endpoint, and the query layer it shares with GraphQL

Date: 2026-10-07. Builds the REST half of ADR 0100 ("list and get, filtering and pagination, with
an OpenAPI document").

## Shared layer: `internal/table`

The GraphQL step had the model, filter compilation, keyset paging, value checks and limits inside
`internal/gql`. REST needs every one of them, and two copies would drift (one protocol accepting a
value the other refuses, or paging differently). They moved, unchanged, to `internal/table`;
`internal/gql` keeps only GraphQL-specific code (SDL, field collection, depth/complexity/root-field
limits, introspection). Every GraphQL test passed unchanged after the move. A consequence worth
knowing: **a cursor from one protocol continues a query in the other**, given the same filter and
order, because both compile to the same SQL (tested).

## The endpoint (`internal/rest`)

```
GET /rows          ?limit=50&after=<cursor>&fields=a,b&order=-a,b&filter[a][gt]=1&filter[b]=x
GET /rows/{key}    ?fields=a,b        single-column primary key only
GET /openapi.json  OpenAPI 3.1, generated from the same model
```

Response shapes: `{"data": [...], "page": {"hasNext": bool, "nextCursor": string|null}}` and
`{"data": {...}}`; errors `{"error": {"message": "..."}}`. Row values serialize exactly as in
GraphQL (bigint/numeric as strings, ISO dates, JSON as JSON).

## Choices to review

1. **Filters are namespaced, `filter[field][op]=value`** (JSON:API style), with `filter[field]=v`
   as `eq`. Bare `field=value` would collide with `limit`, `after`, `fields` and `order` the moment
   a table has a column with one of those names (the test fixture has a column called `limit`).
2. **Unknown query parameters are a 400.** Ignoring `limt=5` or `regoin=x` would silently return
   more data than the caller meant to ask for.
3. **`in` takes the parameter repeated** (`filter[f][in]=a&filter[f][in]=b`), not a comma list, so
   values can contain commas.
4. **AND only.** `or`/`not` are GraphQL-only in v0; nesting them in query strings is awkward and
   GraphQL already offers them on the same data.
5. **Values are strings, format-checked per type**: on top of 0003's custom-scalar formats, `Int`,
   `Float` and `Boolean` get strict formats when they arrive as text (`filter[paid]=yes` is a 400,
   not a cast surprise).
6. **`nextCursor` only when there is a next page**, so a client loops until it is null. (GraphQL's
   `endCursor` follows the connection convention and is set on every non-empty page.)
7. **Status codes**: 400 for anything about the request, 404 for an unknown path or missing row (or
   `/rows/{key}` on a table with no single-column key), 405 for any method but GET/HEAD, 503 when
   the statement timeout cancels the query, 500 "internal error" for anything else (never with
   database text). 401/403 belong to the mounting layer (key verification and the dataset scope
   check) and are documented in the OpenAPI document already.
8. **`Cache-Control: no-store`** on every response: data reached with a per-key credential.
9. **The OpenAPI document** has `servers: [{url: "."}]`, which OpenAPI resolves against the
   document's own location, so it is right wherever the API is mounted. Its version is the
   definition's `generatedAt`, which changes exactly when the columns can. It passes Redocly's
   recommended lint rules in CI (minus `info-license`, `hack/redocly.yaml`).
10. **The management API shows it too**: `GET /api/apis/{id}/openapi`, and a "Show OpenAPI document"
    toggle in the UI, next to the GraphQL schema.

## Not done yet

Mounting both endpoints on core's public routes (ADR 0101) behind key verification and the dataset
scope check, with the workspace's sidecar pool as the database (ADR 0103). Limits become chart
values at that point.
