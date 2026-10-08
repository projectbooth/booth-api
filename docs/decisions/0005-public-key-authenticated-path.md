# 0005: The key-authenticated public path (ADR 0101)

Date: 2026-10-08. Built against booth-core's publicRoutes change (core PR #2, `add-public-routes`,
head `416e0ab`, its `docs/decisions/0016`), read in full before building.

## Shape

The manifest declares `publicRoutes: {pathPrefixes: ["/v1/"]}`. Core serves
`/modules/api/public/v1/...` with no platform login and forwards it here as `/v1/...`
(`internal/public`, mounted at `/v1/`):

```
GET  /v1/{slug}/rows, /v1/{slug}/rows/{key}, /v1/{slug}/openapi.json    REST (0004)
POST /v1/{slug}/graphql   (GET with ?query=&variables=&operationName= also accepted)
Authorization: Bearer booth_ak_<id>_<secret>
```

## Order of checks, and what each returns

1. **Key** (`Authorization: Bearer`, case-insensitive scheme). Missing, malformed, unknown, wrong
   secret or revoked: **401** with `WWW-Authenticate: Bearer`, one message for all of them so the
   response doesn't reveal which. A platform JWT in the header is just a malformed key.
2. **API** in the **key's** workspace by slug: unknown is **404**. Another workspace's API with the
   same slug is invisible (a slug is only looked up in the key's own workspace).
3. **Scope**: the key must name the API's dataset, else **403**.
4. `openapi.json` is served at this point, without touching the workspace database.
5. **Data** as the key's creator (ADR 0103; step 2 of this build). If core refuses to mint for the
   creator: **403** saying why (no current access, or not signed in within the window). No data
   path configured: **503**.

Errors are in the protocol's own shape: `{"error": {"message"}}` on REST paths,
`{"errors": [{"message"}]}` on `/graphql`. `Cache-Control: no-store` throughout.

## `X-Booth-*` is never read here

Core strips `X-Booth-Workspace`/`-Role`/`-Identity` on public routes, but a caller inside the
cluster can reach the module directly and set anything, so `internal/public` doesn't look at them
at all: workspace, scope and owner all come from the verified key. Tested by sending forged values
of all three (they change nothing), both in unit tests and against the deployed pod in the kind
test.

## Choices to review

1. **404 for an unknown slug, 403 for a known one outside the key's scope.** A valid key holder can
   therefore tell which slugs exist in its own workspace. Returning 403 for both would hide that;
   I judged the debugging value worth more than hiding names within the key's own workspace.
   Another workspace's slugs are never revealed either way.
2. **`last_used_at`** (migration 0002) is written at most once a minute per key, off the request
   path, and shown in the key list.
3. **Engines are cached** per API id and `generatedAt`, so a regenerate takes effect on the next
   request; the cache is cleared outright past 256 entries rather than tracking recency.
4. **GraphQL POST bodies are capped at 64 KiB** (413 above that); the query itself is still bound by
   the 16 KiB query limit.
5. **A REST key containing `/`** can't be fetched through the gateway: core decodes and cleans public
   paths before forwarding (its decision 0016 §2), so `/rows/a%2Fb` arrives as `/rows/a/b`. Such a
   row is still reachable with `filter[<key>]=a/b`. Not worked around, since the cleaning is the
   traversal fix.

## Depends on

booth-core PR #2 being merged. The kind test's vendored CRD
(`test/integration/fixtures/boothmodule-crd.yaml`) is copied from that PR's head; re-vendor it from
core's `main` once merged.
