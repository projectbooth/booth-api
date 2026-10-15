# Homelab smoke test: the read path end to end

Checks, on the homelab, the same path the `realstack` CI job checks in kind (ADR 0117 "Reconciled
design" 9a). Steps:
1. Generate an API from a table the Catalog listed by itself (ADR 0116).
2. Read it with a key over REST and GraphQL through `https://booth.home.arpa`.
3. Revoke the key and see 401.

Sign-in tokens can't be fetched from a script: the bundled Keycloak client `booth-design` has direct
access grants off. So generating the API and the key happen on the API page; everything else is a
command.

## Before you start

The API page must be in the shell. That needs booth-design pinning `@projectbooth/api-ui` 0.1.0 and
registering `ApiApp` under the manifest id `api`. Check in the browser:
https://booth.home.arpa/apis should show a page headed **API**, not the "aren't wired into the shell
yet" placeholder.

On server 1 (`homelab-controller`), check the module versions:

```sh
helm list -A | grep -E 'booth-(api|database|catalog)-'
```

Expect `booth-api-0.1.2` (or later), `booth-database-0.1.2` (or later) and `booth-catalog-0.1.5` (or
later) in the CHART column. Older Database or Catalog charts don't auto-list tables.

## 1. Generate an API and a key (browser)

1. Sign in at https://booth.home.arpa as an owner or editor of workspace `home`.
2. **Catalog:** search `nfl_data`. Pick the first table it lists; its row says postgres and was
   listed automatically.
3. **API** page (https://booth.home.arpa/apis): **Generate**, choose that dataset. Note the
   **slug** the new API shows.
4. Same page, **Issue key**: name `smoke-test`, scoped to that dataset. Copy the key. It's shown
   only once.

## 2. Read with the key (terminal, any LAN machine with curl, or server 1)

`--resolve` makes `booth.home.arpa` work even where the hosts entry is missing. `-k` accepts the
self-signed certificate.

```sh
read -r SLUG                  # type the slug from step 1.3
read -rs BOOTH_KEY; echo      # paste the key from step 1.4 (not echoed, not in history)
B="https://booth.home.arpa/modules/api/public/v1/$SLUG"
R="--resolve booth.home.arpa:443:192.168.4.128"

# REST: expect HTTP 200 and {"data":[...three rows...],"page":{...}}
curl -sk $R -w '\nHTTP %{http_code}\n' -H "Authorization: Bearer $BOOTH_KEY" "$B/rows?limit=3"

# GraphQL: asks for the first column of a row; expect HTTP 200 and {"data":{"rows":{"nodes":[...]}}}
COL=$(curl -sk $R -H "Authorization: Bearer $BOOTH_KEY" "$B/rows?limit=1" | grep -o '"data":\[{"[^"]*"' | sed 's/.*{"//; s/"$//')
echo "column: $COL"
curl -sk $R -w '\nHTTP %{http_code}\n' -H "Authorization: Bearer $BOOTH_KEY" -H 'Content-Type: application/json' \
  -d "{\"query\":\"{ rows(first: 3) { nodes { $COL } } }\"}" "$B/graphql"

# The OpenAPI document: expect HTTP 200
curl -sk $R -o /dev/null -w 'openapi.json HTTP %{http_code}\n' -H "Authorization: Bearer $BOOTH_KEY" "$B/openapi.json"

# No key: expect HTTP 401
curl -sk $R -o /dev/null -w 'no key HTTP %{http_code}\n' "$B/rows?limit=1"
```

On server 1, check that the data came through a credential sidecar:

```sh
NS=$(kubectl get deploy -A -l app.kubernetes.io/name=booth-api -o jsonpath='{.items[0].metadata.namespace}')
kubectl -n "$NS" logs -l app.kubernetes.io/name=booth-api --tail=300 | grep -E 'sidecars?:|sidecar\['
```

Expect a `sidecars: started for workspace home …` line.

## 3. Revoke and confirm 401

1. **API** page, **Keys**: **Revoke** `smoke-test`.
2. In the same terminal:

```sh
# Expect HTTP 401
curl -sk $R -o /dev/null -w 'revoked key HTTP %{http_code}\n' -H "Authorization: Bearer $BOOTH_KEY" "$B/rows?limit=1"
unset BOOTH_KEY
```

## If something fails

| You see | Likely cause |
|---|---|
| 403, "creator no longer has access …" | The key's creator lacks a role in `home`, or hasn't signed in within 7 days |
| 503, "reading workspace databases is not available yet" | `core.url` isn't set on booth-api |
| Generate fails naming the catalog schema | The table changed after it was listed; wait for the next listing (5 min) and retry |

Report back:
- the four HTTP codes from step 2 and the one from step 3;
- the sidecar log line;
- anything else, with the full output.
