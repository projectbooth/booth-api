# 0003: GraphQL schema generation, paging and limits: what was built, and where it differs from the note

Date: 2026-10-07. Builds `docs/design-v0.md` §3 (accepted as written). Most of it is exactly the
note; the differences and additions are listed so they can be checked rather than found.

## Built as the note says

- Snapshot, not live: the schema comes from the stored definition; `GET /api/apis/{id}/schema`
  shows it, and the UI has a "Show GraphQL schema" toggle.
- The type mapping table, including `BigInt` and `Decimal` as strings, ISO 8601 strings for dates
  and times, `JSON`, one-dimensional arrays as lists, and unsupported types omitted (listed by the
  schema endpoint and the UI, never converted).
- Name sanitizing (`[_A-Za-z][_0-9A-Za-z]*`, no leading `__`), numbered on collision, deterministic
  from the snapshot so nothing extra is stored.
- One endpoint per API; `rows(where, orderBy, first = 50, after)`, `RowConnection { nodes, pageInfo }`,
  no mutations.
- Limits: `first` defaults to 50 and over 500 is an error (not a clamp); depth 8; complexity
  `first × (selected row fields + 1)` summed over root fields, cap 10,000; 5 root fields; filter
  nesting 4; `in` lists 100; statement timeout 5s. All checked before any SQL runs.
- Every request runs in a `READ ONLY` transaction with `SET LOCAL statement_timeout`.

## Differences from the note

1. **`startsWith`, not `like`, on `StringFilter`.** The note said "`like` (anchored prefix match
   only)". A field called `like` that doesn't take LIKE patterns would mislead; `startsWith` says what
   it does. `%`, `_` and `\` in the value are escaped, so it can't become an unanchored scan.
2. **`row(<key column>: <its type>!)`, not `row(id: ID!)`.** The argument is named and typed after
   the real single-column primary key (`row(id: BigInt!)` for a bigint `id`), which is what a client
   needs to type it. Still only offered for single-column keys.
3. **Ordering is limited to `NOT NULL`, comparable columns** (enum `RowField`). Keyset cursors
   compare with `<`/`>`, which NULLs break. The primary key is always appended as the tiebreak.
4. **No `ctid` in offset mode.** The note said "`ctid` order with offset fallback" for tables
   without a key. Views and foreign tables have no `ctid` (a test found this), so offset mode orders
   by the requested columns and then every comparable column. The caveat is unchanged: offset pages
   can skip or repeat rows under concurrent writes. Keyset mode needs a primary key whose columns
   are all exposed and orderable.

## Additions the note didn't cover

5. **`not` in `RowFilter`**, alongside `and`/`or`. Cheap and expected; counted in filter depth. A
   column named `and`, `or` or `not` gets a trailing underscore so it can't collide.
6. **Custom scalar inputs are format-checked before Postgres sees them.** Postgres's casts also
   accept `'yesterday'`, `'now'`, `'infinity'` for timestamps and `1e3` for numeric (a test found
   the first). Inputs must be ISO 8601 (`Date`, `DateTime` with optional offset, `Time`), an integer
   string for `BigInt`, or a plain decimal for `Decimal`. In-format but out-of-range values (a
   20-digit bigint) are still refused by Postgres and reported as "isn't valid for its column".
7. **Cursors are bound to their filter and ordering.** A cursor carries a fingerprint of `where`
   and `orderBy`; replaying it under different ones is "invalid cursor", rather than silently
   paging the wrong result.
8. **Introspection has its own depth limit (15).** The standard introspection query nests `ofType`
   about eight levels below `__schema`, over the data limit of 8; introspection doesn't touch the
   database, so a separate bound is enough. Introspection fields don't count as root fields or
   toward complexity.
9. **Errors never carry SQL or database messages.** A data-exception (`22xxx`) from a bad value is
   reported as "a value in the query isn't valid for its column: <Postgres's message>" (which names
   the type and value, not the statement); anything else is "internal error", passed to
   `Engine.OnInternalError` for logging.
10. **Generation refuses a table with no servable column** (all `bytea`, say) with 422, rather than
    storing an API whose schema can't exist.

## Not done yet

- Mounting: no route serves this. It goes on core's public routes (ADR 0101) behind key
  verification, with the per-workspace sidecar pools (ADR 0103) as the `DB`.
- The limits are package defaults (`gql.DefaultLimits`); they become chart values when the endpoint
  is mounted.
