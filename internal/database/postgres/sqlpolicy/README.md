# SQL compiler boundary

`Parse` applies an independent lexical budget before calling the pinned native
PostgreSQL parser, then checks every populated protobuf field and AST budget.
`Parsed.Compile` resolves names through lexical scopes, obtains physical relation
facts through the driver's catalog adapter, checks exact types, and emits SQL.
Original SQL is never passed to the catalog adapter or retained in a plan.

The compiler supports one SELECT, explicit schema-qualified tables, aliases and
column alias lists, stars, typed literals/parameters, INNER/LEFT/RIGHT/FULL/CROSS
joins with ON, boolean/comparison/numeric expressions, NULL predicates, BETWEEN,
finite IN lists, noncorrelated EXISTS/IN subqueries, derived SELECTs, nonrecursive
CTEs, simple GROUP BY/HAVING, count/sum/avg/min/max (including supported DISTINCT
arguments), expression/output-alias/ordinal ordering, and integer LIMIT/OFFSET. Grouped
expressions reuse their validated bindings. Internal range/column names are
unique; output labels, including duplicates, preserve PostgreSQL semantics.

Unknown strings and NULL need a supported contextual or explicit type. Numeric
casts use exact catalog signatures; implicit widening follows int2 → int4 →
int8 → numeric. Other mixed-type coercions fail closed. Comparison, ordering,
min/max, and DISTINCT require an available exact built-in signature (for example,
json has no equality operator). Non-numeric casts of already typed expressions,
type modifiers, ambiguous output-alias ordering, grouping aliases/ordinals, SELECT
DISTINCT, explicit CTE materialization, backslash escapes in quoted input,
correlated/lateral queries, set operations, arrays, windows, custom functions and
operators, and unhandled AST fields remain unsupported. Use an expression or
projection ordinal for ORDER BY and typed parameters for escaped text.

Budgets are 64 KiB SQL, 8,192 lexical tokens and AST messages, depth 64,
256 supplied parameters (256 KiB total text, 64 KiB each), 8,192 generated bindings, 1 MiB cumulative expression
emission, 1 MiB final SQL, 4,096 catalog relations, and 8,192 catalog columns.
Native worst-case parsing runs in a deadline-controlled subprocess regression.
No claim is made that Go context cancellation interrupts a native parser call.

`catalog16.json` and `catalog18.json` are embedded semantic manifests from owned
PostgreSQL 16.15 and 18.6 official Docker fixtures. `catalog.sql` explicitly
projects the reviewed fields with `search_path=pg_catalog`; function/operator
references are numeric OIDs, never context-dependent `regproc` names.
`catalog_records.go` is the typed field contract shared by verification and
compiler lookup. The manifests include:

- Supported scalar identities and I/O, typmod, analyze and subscript callbacks.
- Same-type comparison/numeric operators, unary minus and planner callbacks.
- Aggregate signatures and transition/final/combine/serialization helpers.
- Numeric cast implementations, methods and coercion contexts.
- Built-in btree opclasses, operator-family members and support functions.
- Exact implementation metadata for referenced functions, recursively including
  their planner support callbacks.

Every previously compared property is retained except these explicit allowances:

| Category | Omitted properties | Semantic identity |
| --- | --- | --- |
| Casts | Row `oid` | Source and target type |
| Operator classes | Row `oid` | Namespace, access method and name |
| Family members | Row `oid` | Family, operand types, strategy and purpose |
| Support entries | Row `oid` | Family, operand types and support number |
| Functions | `procost`, `prorows` | Stable function OID and full definition |
| Aggregates | `aggtransspace`, `aggmtransspace` | Stable aggregate function OID |

Collections compare by identity, independently of row order. Stable built-in
OIDs, implementation bindings, security/strictness/volatility/parallel flags,
function configuration, aggregate semantics and all other retained fields must
match. Numeric planner estimates are not callbacks: planner callback identities
and implementations remain verified. Owner/ACL fields remain excluded because
role/privilege checks are separate. Operator-class row OIDs are resolved through
live catalog joins, including when checking a table's indexes.

Each major's embedded policy is validated and indexed once with `sync.OnceValues`.
Only this private immutable policy is cached. Every compilation fetches and
verifies live metadata; no authorization result survives a request. Input is
bounded to 2 MiB, with duplicate JSON keys, unknown/missing fields, malformed OIDs,
missing/duplicate/unexpected records and dangling implementation references
rejected. Presence checks distinguish an omitted field from false, zero or null.
Unsupported majors and changed semantic definitions fail with `QUERY_UNSUPPORTED`.

The emitter's SQL subset remains fixed: a referenced function's presence does
not make it callable by user SQL. Default, C and POSIX collations are allowed;
application collations and custom/expression/partial/non-btree indexes are not.

To regenerate review candidates, use the existing owned fixture harness:

```sh
candidate_dir="$(mktemp -d /tmp/data-mate-catalog.XXXXXXXX)"
make test-integration DB_DRIVER=postgres CATALOG_OUTPUT_DIR="$candidate_dir"
```

Export mode writes `catalog16.json`, `catalog18.json` and per-major provenance
files containing server versions and image digests. The directory must already
exist beneath `/tmp`; existing output files are never overwritten. It uses the
runtime extraction SQL and normal fixture ownership/cleanup checks. It does not
run acceptance tests or update tracked manifests. Review the candidates against
the typed schema and previous manifests before replacing embedded policy; never
learn an allowlist from a user's database. Then run both acceptance commands:

```sh
make test-integration DB_DRIVER=postgres DB_IMAGE=postgres:16
make test-integration DB_DRIVER=postgres DB_IMAGE=postgres:18
```

The trusted adapter freezes inheritance/partition scans into explicit ONLY
relations, using UNION ALL for a hierarchy and typed empty projections for an
empty partition root. All ordinary inheritance descendants must be in scope;
partition-root selection includes its partitions. Hierarchies with RLS are
rejected; ordinary noninherited RLS remains inside the documented trusted-server
boundary. Stable-fixture equivalence is exercised on both majors.

A compiled plan is not execution authorization. P5 must acquire locks, recheck
names/OIDs/columns/hierarchy and built-in identities, enforce result bounds,
execute using explicit parameter OIDs, and test DDL races. Production `Query`
continues to return an explicit failure. Only the owned integration oracle
executes emitted statements in P4, comparing raw text rows, types and labels to
fixture queries. No agent SQL is prepared or executed during compilation.

References: [PostgreSQL operator resolution](https://www.postgresql.org/docs/16/typeconv-oper.html)
and [the pinned parser's typed API](https://github.com/pganalyze/pg_query_go/tree/v6.2.2).
