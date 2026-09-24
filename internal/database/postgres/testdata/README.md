# Version 1 fixture contracts

`codecs.json` pairs canonical PostgreSQL type names and nullable text-protocol
values with exact JSON values. JSON numbers must be read with `UseNumber`;
int8/numeric are strings. `encoding` marks bytea base64. Local timestamps omit
zones; timestamptz is UTC; BC and infinity remain explicit strings. All types
include SQL NULL; bytea columns retain their encoding marker even for NULL.
The offline codec regression and owned PostgreSQL 16/18 scenario execute these
values as separately bound parameters with SQL casts and as stored columns.

The owned query scenario also covers mixed enum/array tables, nested arrays,
domains, text fallbacks, view/function dependencies and column-level grants.
The query guard has its own bounded syntax/scope corpus; no semantic manifests
or signature allowlists are maintained.
