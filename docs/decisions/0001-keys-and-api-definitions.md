# 0001: API keys and API definitions, first build

Date: 2026-10-07. Builds ADR 0100 (keys), ADR 0102 (catalog shape, against a fixture) and the
snapshot part of `docs/design-v0.md` §3. Judgment calls made here that no ADR fixes are listed
under "Choices to review".

## What exists

- **Management API** at `/api/*` (reached as `/modules/api/api/*` through core's gateway), OIDC
  verified with the role re-derived from the token (ADR 0041; `internal/auth`, copied from
  booth-database, which copied booth-storage):
  - `GET /api/apis`, `GET /api/apis/{id}`: any role.
  - `POST /api/apis {datasetId}`, `POST /api/apis/{id}/regenerate`, `DELETE /api/apis/{id}`:
    editor or owner.
  - `GET /api/keys`, `POST /api/keys {name, datasetIds}`, `POST /api/keys/{id}/revoke`: editor or
    owner.
- **Definition flow** (`internal/apis`): fetch the dataset from booth-catalog through core's gateway
  as the caller → require `format: "postgres"` → introspect the real table → reconcile with the
  catalog's schema → store the snapshot (`internal/store`, own database, ADR 0053).
- **Keys** (`internal/keys`): `booth_ak_<13-char id>_<43-char secret>`, SHA-256 of the secret
  stored, shown once (`Cache-Control: no-store` on that one response), scoped to dataset ids,
  revocable. `Verify` exists and is tested but **is not mounted on any route**: generated endpoints
  are served on core's public routes (ADR 0101), which wait for core.
- **UI** (`@projectbooth/api-ui`): APIs with their column snapshots, generate from a dataset,
  regenerate/delete, issue a key (secret shown once with a copy button), list, revoke. States the
  7-day owner limit (ADR 0103 item 1) and that endpoints aren't live yet.

## What is deliberately not built

- **No data source in production.** `source.Unavailable` is wired in `cmd/api`, so generating an
  API answers 503 "reading workspace databases is not available yet". The per-workspace sidecar
  pools (ADR 0103) wait for core to confirm the minting bound (ADR 0103 item 4); the manifest does
  not declare `workloadIdentity`, and a contract test pins that.
- **No public routes.** No `publicRoutes` in the manifest (pinned by a contract test), no
  key-authenticated handler.
- **The catalog's postgres field name is provisional.** booth-catalog hasn't built ADR 0102 yet.
  The table block is read from `postgresTable` (`catalog.PostgresTableField`, one constant). When
  the catalog reports its real field name, change that constant and the test fixture.

## Choices to review

1. **SHA-256, not bcrypt/argon2, for the stored hash.** The secret is 256 random bits; slow hashes
   exist to protect low-entropy passwords from guessing and add nothing here except per-request
   cost. Compared in constant time.
2. **Viewers can list APIs but not keys.** A viewer can't issue or revoke, so the list of keys,
   their scopes and who made them is not theirs to need. APIs (dataset, table, columns) are no
   more than the catalog already shows them.
3. **Scope is stored by dataset id, and deleting an API removes that dataset from every key's
   scope.** Regenerating keeps keys working; delete-then-recreate does not silently re-enable old
   keys. A key left with no datasets stays listed ("none") and grants nothing.
4. **A dataset registered with no schema in the catalog can still get an API** (no mismatches to
   report; descriptions empty). A non-empty catalog schema must name exactly the table's columns,
   or generation is refused with the differences listed, per `docs/design-v0.md` §3. ADR 0102 item 4
   calls the catalog schema "descriptive only"; this check is about the API not contradicting the
   catalog, not about trusting its types, which always come from the table.
5. **A table the lease can't SELECT from is "not found"**, the same as a missing one, so the error
   reveals nothing about tables outside the workspace's grant.
6. **Slugs** come from the dataset name (`orders`, then `orders-2`), unique per workspace, fixed at
   creation (regeneration keeps the slug even if the dataset is renamed), because they will be URL
   path segments of the public endpoints.
7. **No `last_used_at` yet.** It needs the verification path to be live; added with the public
   routes.
