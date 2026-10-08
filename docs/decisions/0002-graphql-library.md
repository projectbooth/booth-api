# 0002: GraphQL library: vektah/gqlparser plus our own executor

Date: 2026-10-07. The spike `docs/design-v0.md` §3 promised before choosing.

## Question

Generated APIs need a GraphQL schema built at runtime from a stored table snapshot. gqlgen
generates Go code at build time, so it is out. The two candidates named in the note were
`graphql-go/graphql` (builds and executes a runtime schema) and `vektah/gqlparser/v2` (parses and
validates against a schema; no executor) with booth-api's own small resolver.

## What the spike did

Both were run on the same generated schema (`Row`, `RowConnection`, a `rows(first:)` field, a
custom `BigInt` scalar) with the same query using an alias, a variable, a named fragment and a
directive, plus an invalid field and an introspection query. The spike code was throwaway (kept out
of the repo); the results:

| | graphql-go v0.8.1 | gqlparser v2.5.60 |
|---|---|---|
| Runtime schema | `graphql.NewSchema` from Go structs | `LoadSchema` from generated SDL text |
| Validation errors | spec-shaped, with locations | spec-shaped, identical message for the same error |
| Variables | coerced during execution | `validator.VariableValues` coerces before anything runs |
| Seeing the whole query before execution | resolver gets raw AST; fragment spreads must be looked up by hand (the spike printed `...fragment F (must resolve via p.Info.Fragments)` instead of the column) | spreads already resolved to their definitions on the AST; each field carries its schema definition |
| Introspection | built in, worked | validates; answering it is ours to write |
| Last release | v0.8.1, **2023-04-10** | v2.5.60 2026-10-02 (v2.5.61 2026-10-07); it is gqlgen's parser, actively maintained |

## Decision

**gqlparser, with booth-api's own executor** (`internal/gql`).

- Maintenance is the deciding factor. These endpoints will take unauthenticated-at-the-edge traffic
  (ADR 0101, public routes) parsed by this library; a parser with no release in three and a half
  years is the wrong thing to put there. gqlparser is maintained because gqlgen depends on it.
- ADR 0100 requires depth and complexity limits before execution. With gqlparser the validated,
  fragment-resolved AST and coerced variables are available up front, so limits are checked on
  exactly what will run, and each `rows` field compiles to one SQL statement selecting only the
  requested columns. graphql-go's per-field resolver model gives neither for free.
- The cost is writing execution and introspection. The schema shape is fixed and small (one object
  type plus connection/page wrappers, no interfaces, unions or mutations), so the executor is a few
  hundred lines; introspection is answered from gqlparser's own `ast.Schema`, and the full
  introspection query graphql-js/GraphiQL send is a test (`TestIntrospection`).

Pinned at v2.5.60 rather than the day-old v2.5.61.

## Revisit if

The schema grows relations between datasets (joins, nested types), or the executor starts
accumulating spec corner cases. Then a full executor library becomes worth its weight again; check
gqlgen's runtime-schema story at that point.
