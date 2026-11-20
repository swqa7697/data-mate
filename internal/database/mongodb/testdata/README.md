# MongoDB codec fixtures

`codecs.json` defines the exact canonical Extended JSON produced for each
supported BSON value and the representations the driver refuses. The offline
codec test decodes each case through the production encoder. The Docker
integration test stores the seedable cases in a collection and reads them back
through `find_documents`.

| Field | Meaning |
| --- | --- |
| `name` | Unique case identifier, used as the seeded document `_id` |
| `extjson` | One Extended JSON value, tested as the document `{"v": value}` |
| `bson_hex` | A whole BSON document in hexadecimal, for values Extended JSON cannot express |
| `json` | The expected value for `extjson`, or the expected whole document for `bson_hex`, compared after compaction |
| `unsupported` | The document must be refused as `QUERY_UNSUPPORTED` instead of encoded |
| `seed` | Optional, `extjson` cases only; `false` keeps a case out of the live fixture |

Cases are seeded by default. Undefined, symbol, DBPointer and code-with-scope
values are deprecated BSON types that the fixture does not insert, and
`bson_hex` cases are offline only.

Representation rules:

- Output is canonical Extended JSON: every number keeps its BSON type, including
  `{"$numberInt": …}`, `{"$numberLong": …}`, `-0.0`, NaN, infinities and
  Decimal128 trailing zeros; dates are `{"$date": {"$numberLong": …}}`.
- Key order and duplicate keys are preserved, as are empty, dotted and
  `$`-prefixed keys.
- Every key, string, symbol, code and regular expression must be valid UTF-8.
  Invalid UTF-8 is refused rather than replaced.
- A DBPointer namespace must be valid UTF-8 without `"`, `\` or control
  characters, because the Extended JSON writer emits it without escaping: a
  quote could inject fields while the JSON stays valid.
- A legacy subtype `02` binary must repeat a length equal to its outer length
  minus 4. A shorter inner length would leave bytes that a reader emits as
  forged sibling fields.

`seed.js` is the static mongosh seed for the Docker integration fixture.
`TestMongoDBIntegration` runs it once inside the owned container against
`mongodb://127.0.0.1:27017/admin`. Before the file, the test authenticates as
root and declares `const codecCases`: the cases in this corpus that have
`extjson` and are not marked `"seed": false`. Passwords are read from `/fixture`
through `fs`, never from arguments. mongosh converts Extended JSON through
JavaScript values, so a case that depends on exact number text, duplicate keys,
integer-like key order or an unrepresentable date must set `"seed": false`.
