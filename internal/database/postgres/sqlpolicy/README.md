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

`catalog16.json` and `catalog18.json` are exact, embedded catalog snapshots from
owned PostgreSQL 16.15 and 18.6 official Docker fixtures. `catalog.sql` is the
constant query used to extract and verify them with `search_path=pg_catalog`.
Each snapshot includes:

- Supported scalar type identities and their input/output/send/receive functions.
- Same-type comparison/numeric operators and numeric unary minus.
- Aggregate signatures, transition/final/combine/serialization functions.
- Numeric cast implementations.
- Built-in btree opclasses, operator-family members and support functions.
- Exact implementation metadata for the referenced functions.

The emitter still selects only the finite forms above; presence of a function in
a snapshot does not expose that function to user SQL. Verification compares the
whole snapshot before compilation, rather than allowing catalog discovery to
extend policy. A different major or catalog signature fails closed. Snapshot
changes require review and both-major integration; do not regenerate an allowlist
from an arbitrary user's database. Snapshot owner/ACL fields are omitted because
role/privilege checks are separate. Default, C and POSIX collations are allowed;
application collations and custom/expression/partial/non-btree indexes are not.

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
