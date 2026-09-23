# Version 1 fixture contracts

`codecs.json` pairs canonical PostgreSQL type names and nullable text-protocol
values with exact JSON values. JSON numbers must be read with `UseNumber`;
int8/numeric are strings. `encoding` marks bytea base64. Local timestamps omit
zones; timestamptz is UTC; BC and infinity remain explicit strings. All types
include SQL NULL. These are downstream acceptance inputs, not a working codec.

`signatures.json` retains the P0 fixture-schema examples. It is not the runtime
allowlist. P4 embeds complete per-major type/operator/aggregate/cast/btree and
function identities in `../sqlpolicy/catalog16.json` and `catalog18.json`, and
verifies them through `catalog.sql` before compilation. See the
[compiler contract](../sqlpolicy/README.md) for supported forms, snapshot provenance
and P5 execution requirements.
