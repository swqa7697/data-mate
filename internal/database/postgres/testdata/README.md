# Version 1 fixture contracts

`codecs.json` pairs canonical PostgreSQL type names and nullable text-protocol
values with exact JSON values. JSON numbers must be read with `UseNumber`;
int8/numeric are strings. `encoding` marks bytea base64. Local timestamps omit
zones; timestamptz is UTC; BC and infinity remain explicit strings. All types
include SQL NULL. These are downstream acceptance inputs, not a working codec.

`signatures.json` freezes the per-major catalog fixture shape: kind, exact
namespace/name, argument types, result type, and implementation identity.
The two addition examples do not constitute a compiler allowlist. P4 must fill
and verify complete signatures against PostgreSQL 16 and 18, including aggregate
transition/final functions and cast implementations, before authorizing SQL.
