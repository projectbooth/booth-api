# 0006: Reading workspace data through per-identity credential sidecars (ADR 0103)

Date: 2026-10-08. Builds ADR 0103 on booth-core's credential sidecar (ADR 0095,
`contracts/credential-sidecar.md`) and workload minting (ADR 0056/0058). Core confirmed (its
`internal/workload/service.go`, `ownerRole`) that minting refuses unless the owner holds a current
role in that workspace, which ADR 0103 item 4 required before booth-api could declare
`workloadIdentity: {mint: true}`. I read that code before relying on it.

## How a request reaches the data

1. The public path (0005) has verified the key: workspace W, creator C.
2. `sidecars.Manager.Pool(W, C)`: if no sidecar is running for (W, C):
   - mint a workload token from core (`subject: apikeys:W`, `roleCeiling: viewer`, `owner: C`);
     a 403 naming the owner becomes `source.ErrOwnerNoAccess`, and the request gets a 403 that says
     why;
   - write it, 0600, to `<dir>/token` in a fresh 0700 directory under an in-memory emptyDir;
   - start `/credential-sidecar --kind=postgres --access=read --scope={"workspace":"W"}
     --workspace=W --token-file=<dir>/token --listen=unix://<dir>/.s.PGSQL.5432 --core-url=...`
     with an **empty environment**;
   - wait for its `/healthz` (served on the same socket) to report a lease;
   - open a pgx pool with `host=<dir>`.
3. The query runs in a read-only transaction with a statement timeout, against only the table in
   the API's stored snapshot (0003/0004: identifiers come from the snapshot, never the request).

The sidecar binary is copied into booth-api's image from
`ghcr.io/projectbooth/credential-sidecar@sha256:6a0a795e…ce14`, the digest booth-pipeline and
booth-notebooks pin (a contract test checks it stays digest-pinned).

## Lifecycle

- **Renewal**: the token is re-minted when two thirds of its life has passed (about every 6.7
  minutes for core's 10-minute tokens). A refusal stops the sidecar at once, so a key stops working
  within one token lifetime of its creator losing access or passing core's recency window. A
  transient failure is retried until the current token expires.
- **Idle shutdown**: no request for `dataAccess.idle` (default 10 minutes) stops the process, closes
  the pool and removes the directory, token included. The next request starts it again (a few
  seconds for the first lease).
- **Crashes**: an unexpected exit is logged and the entry dropped; the next request starts a new
  sidecar.
- **Refusals are remembered for 30 seconds**, so a burst of requests on a dead key costs one mint
  call, not one per request.
- **Connections** are recycled after 15 minutes, inside the sidecar's guaranteed minimum (about
  half of booth-database's one-hour lease), with 30-second health checks. If starting a transaction
  fails on a dropped connection, it is retried once (`table.WithTx`); nothing inside a transaction
  is retried.

## Choices to review

1. **One sidecar per (workspace, key creator), not per workspace.** ADR 0103 says "one sidecar
   process per workspace", and also that a key stops working a week after *its* creator last signs
   in. Both can't hold with one token per workspace: that token names one owner, so every other
   creator's keys in the workspace would ride on that one person's access, including keys whose own
   creator had lost it. I kept the per-key bound and run one process per (workspace, creator). The
   lease is identical either way (read, on the workspace database), so the cost is processes, which
   the idle shutdown bounds. **If you'd rather have one per workspace, the creator check needs
   another mechanism; tell me and I'll change it.**
2. **Generating an API introspects as the person generating it** (owner = their `sub`), so they
   see exactly the read access a key they issue would get.
3. **The sidecar gets an empty environment.** booth-api's own environment holds its database DSN
   and its minting credential; nothing the sidecar needs is there (everything is a flag), so
   nothing is passed.
4. **Data access is enabled by `core.url`.** Setting it makes the chart declare
   `workloadIdentity.mint`, read `booth-workload-minting-credentials` and mount the in-memory
   directory. Without it the module still serves keys, scope and schema documents, and data
   requests answer 503.
5. **Network**: booth-api has no NetworkPolicy of its own, and its namespace carries
   `booth.projectbooth.io/database-client=true` (core labels the namespace of every module that
   declares `database`), which booth-database's ingress policy admits. So no chart egress rule was
   needed, unlike booth-notebooks and booth-pipeline (ADR 0092).

## Tested here, and where the rest is tested

`internal/sidecars` runs real child processes, real Unix sockets and a real pgx pool, with a fake
sidecar binary (the test binary re-executed) that records its flags and relays to the test
Postgres. It covers the exact flags, the empty environment, token file and directory modes, one
process per (workspace, owner), refusal without starting a process, the refusal window, renewal,
stopping on a refused renewal, idle shutdown and restart, crash replacement, a failing start, and
`Close`. The real sidecar against real booth-core and booth-database is the next step's
integration test.
