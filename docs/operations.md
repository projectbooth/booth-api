# Operating booth-api

For an operator installing booth-api with Helm, for example on a homelab. How to use an API once
it's running is in the README ("Using a generated API").

## What it needs

- **booth-core**, **booth-catalog** and **booth-database** installed. booth-database's credential
  broker provider is on by default; it's how booth-api reads workspace tables.
- booth-core provisions two Secrets into booth-api's namespace. **You don't create either one.**
  - `booth-database-credentials` (`dsn`): booth-api's own database, for keys and API definitions.
    Core writes it because the manifest declares `database: {enabled: true}` (ADR 0053).
  - `booth-workload-minting-credentials` (`url`, `credential`): written once `core.url` is set
    (below), because the manifest then declares `workloadIdentity: {mint: true}` (ADR 0103).

  Until core has written a Secret, the pod waits in `CreateContainerConfigError`. That's normal
  for a minute after install. If it lasts, check that booth-core's controller sees the
  `BoothModule` (`kubectl get boothmodules -A`).

## Chart values to set

| Value | Set to | Without it |
|---|---|---|
| `core.url` | booth-core's in-cluster URL, e.g. `http://booth-core.booth-system.svc:8080` | Generating an API fails, and every data request on the public route answers 503 ("reading workspace databases is not available yet"). Keys and the schema documents still work. |
| `oidc.issuerUrl`, `oidc.clientId` | The same identity provider and client booth-core uses (e.g. Keycloak's realm URL and `booth-design`) | The management API (the API page in Booth) answers 503. |
| `oidc.groupsClaim` | Only if booth-core's isn't `groups` | Roles aren't recognised; everyone gets 403 on the management API. |
| `oidc.jwksUrl` | Optional (ADR 0108): where to fetch the identity provider's signing keys instead of using discovery, e.g. `http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect/certs` | Discovery from `oidc.issuerUrl`, as before. |
| `dataAccess.idle` | How long an unused sidecar keeps running (default `10m`) | Defaults apply. |

**`oidc.jwksUrl` (ADR 0108).** When set, booth-api fetches the provider's signing keys directly from
this URL and never contacts the issuer's discovery document. Tokens are still accepted only if their
`iss` equals `oidc.issuerUrl` exactly. Setting it without `oidc.issuerUrl` is a startup error. At
startup booth-api logs `oidc: verifying tokens with issuer=… keys-from=…` once. The bundled install
points it at Keycloak's in-cluster Service over plain http, so pods never dial the Ingress or need to
trust its certificate. **The trust assumption:** that key fetch is in-cluster, unauthenticated and
unencrypted, so it relies on NetworkPolicy and on trusting the cluster network. Anyone who could
answer that URL could issue tokens booth-api accepts. Leave it empty when the identity provider is
external.

Setting `core.url` is what turns data access on. With it, the chart declares
`workloadIdentity.mint`, reads `booth-workload-minting-credentials`, and mounts an in-memory
directory at `/run/booth-api` for the sidecars' sockets and token files.

Example:

```sh
helm upgrade --install booth-api charts/booth-api -n booth-api --create-namespace \
  --set core.url=http://booth-core.booth-system.svc:8080 \
  --set oidc.issuerUrl=https://keycloak.example/realms/booth --set oidc.clientId=booth-design
```

## The sidecar processes

booth-api reads each workspace's database through booth-core's **credential sidecar** (ADR 0095),
run as **child processes inside booth-api's own pod**, not as separate containers. There is one
per workspace and key creator that has been used recently (ADR 0103, docs/decisions/0006).

- Each listens on a Unix socket in a private directory under `/run/booth-api/sidecars/`. No
  network port is opened.
- Each holds a workload token that core mints for the key's creator (viewer at most), and asks
  core's broker for a read-only database lease with it. booth-api never sees a database password.
- They start on the first request for that workspace and creator. Expect a few seconds the first
  time.
- They stop after `dataAccess.idle` with no request, and are replaced if they crash.
- Their log lines appear in booth-api's own log, prefixed `sidecar[<workspace>]:`, along with
  `sidecars: started for workspace …` / `stopped …`.

The binary comes from `ghcr.io/projectbooth/credential-sidecar`, pinned by digest in booth-api's
Dockerfile.

## When a key's creator hasn't signed in for 7 days

A key reads data as the person who created it. booth-core refuses to mint a token for someone it
hasn't seen sign in within `workloadIdentity.ownerMaxAge` (default `168h`, 7 days), or who no longer
has a role in that workspace. The key's requests then answer **403** with:

> this key's creator no longer has access to the workspace, or hasn't signed in to Booth within the
> last 7 days (the default); ask them to sign in, or issue a new key

The creator signing in to Booth again fixes it. The next request mints again, so allow up to 30
seconds, because booth-api briefly remembers a refusal. If a sidecar was already running for that
creator, it stops at its next token renewal, within about 7 minutes.

To make the window longer, for example when keys belong to people who rarely open Booth, raise it
**in booth-core's chart**, not booth-api's:

```sh
helm upgrade booth-core <booth-core chart> -n booth-system --reuse-values \
  --set workloadIdentity.ownerMaxAge=720h   # sets BOOTH_WORKLOAD_OWNER_MAX_AGE in core
```

This applies to every module that mints workload tokens, not just booth-api. It is also how long
someone removed from your identity provider, who never signs in again, keeps their keys working.

## Known limits

- **No rate limiting** (ADR 0100). The public route (`/modules/api/public/v1/...`) proxies requests
  nobody has logged in for, and only page size, query cost and a 5-second statement timeout bound
  them. **Don't expose Booth to the internet** until rate limiting is decided (ADR 0101).
- **REST primary keys containing `/`.** booth-core cleans public paths before forwarding them, so
  `/rows/a%2Fb` can't reach a row whose key is `a/b`. Use `/rows?filter[<key column>]=a%2Fb`
  instead (with curl, add `-g`).
- **Slug names are visible inside a workspace.** Any valid key gets 404 for an API name that
  doesn't exist in its workspace and 403 for one that exists but is outside its scope, so it can
  tell which API names exist in its own workspace. It learns nothing about other workspaces.
- **One table per API, read-only**, and Postgres tables from booth-database only. Iceberg tables
  and files are out of v0 (ADR 0100, `ARCHITECTURE.md` item 53).
