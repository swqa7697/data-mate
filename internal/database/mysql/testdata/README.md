# MySQL and MariaDB codec fixtures

`codecs.json` defines the exact JSON produced for each supported result type.
The offline codec test decodes `wire`, the client library's binary-protocol
value, through the production codec. The Docker integration test stores
`literal` in a column of type `column` on every server in the matrix and checks
the same JSON, result type name and encoding through `query`.

| Field | Meaning |
| --- | --- |
| `column` | Column DDL type used by the integration fixture |
| `literal` | SQL literal stored in that column |
| `library` | The client library's type name for the result column |
| `wire` | One of `int64`, `float32`, `float64` (decimal text), `bytes` (UTF-8 text), `hex` (raw bytes) or `null` |
| `type` | The result column `type` reported to agents |
| `json` | The exact JSON value, compared after compaction |
| `encoding` | Optional value encoding marker, such as `base64` |
| `flavors` | Optional subset of `mysql` and `mariadb`; MariaDB stores JSON as text |

Representation rules:

- `BIGINT`, `BIGINT UNSIGNED`, `DECIMAL` and `BIT` values are exact decimal strings.
- Other integers and `YEAR` are JSON numbers; `FLOAT` keeps 32-bit precision.
- `DATETIME` uses `T` without a zone; `TIMESTAMP` is UTC with `Z`.
- Zero dates keep their server spelling.
- Binary strings, spatial values and unknown types are base64.
- MySQL `JSON` keeps exact numbers and member order as the server returns them.
