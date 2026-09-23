# Data Mate

A checkout-local Go CLI for sharing PostgreSQL connections with terminal agents
through a read-only MCP service on macOS with Apple Silicon.

**Current implementation: P7 scope selection and connection diagnostics.**
`db add`, `db edit`,
`db remove`/`rm`, `db list`/`ls`, `db scope`, and `db test` use the profile/vault store. Interactive
forms, scripted input, strict profiles, atomic publication, AES-256-GCM, durable
encryption accounting, and one native Keychain item per installation are
implemented. The internal PostgreSQL driver supports direct, SSH and SOCKS5
routes with optional verified TLS,
read-only role checks, scoped catalog pages and table descriptions. The internal
compiler validates a finite SELECT subset against exact PostgreSQL 16/18 catalog
signatures and executes only emitted, parameterized SQL after relation locking
and fresh authorization checks. The MCP service and agent registration remain later packages; their commands
fail explicitly.
`upgrade` and `update` explain how to rebuild locally and make no network request.

Install Go 1.27.1 and Apple's Command Line Tools (`xcode-select --install`). Then:

```bash
make install
make dev ARGS="help"
make dev ARGS="version"
```

`make install` downloads the pinned Go modules, shfmt, and staticcheck and builds
`.dev/bin/data-mate`. Dependency checks provide installation guidance; they never
install system packages. Go 1.27.1 is selected explicitly by the scripts. Native
cgo and the macOS SDK are required. The binary reports the application version
from `VERSION`, plus revision and working-tree state when Git metadata is present.

Use `make setup` to prepare the same dependencies/tools without compiling the
application or creating `.dev`. Add `VERBOSE=1` to `make setup`, `make install`, or
`make build` to show Go download/build commands when those operations run.

The installed binary resolves its root from its executable location, so it works
from another working directory. An absolute `--root` overrides that location.
Each checkout has its own `.dev`; installation creates only its private `bin`
directory and executable. The internal store initializes persistent identity and
state locks after a confirmed mutation or the first `db test` of manually written
profiles. Listing, previews and canceled forms do not initialize that store. Install/build do not initialize it either. Service
lifecycle integration arrives in P8. Builds replace the executable
atomically. No service, agent registration, or Keychain item is created during installation.

Manage connections interactively:

```bash
make dev ARGS="db add"
make dev ARGS="db edit"                # Select a saved connection.
make dev ARGS="db list --json"
make dev ARGS="db scope analytics"      # Optional; new connections default to all.
make dev ARGS="db test --json"           # Test every saved connection.
make dev ARGS="db remove analytics"
```

The basic form asks for driver, alias, host, port, database, visible username,
and hidden password, followed by a nonsecret preview and one default-No Y/N
confirmation. Blank edit fields preserve values; a blank edit password keeps the
existing secret. Use `--clear-password` to remove it. Cancellation exits 130 and
leaves files and Keychain untouched. Human previews honor `NO_COLOR`; JSON listing
is version 1, contains no credential references, and never accesses Keychain.

For scripts, supply complete fields and `--yes`. This example explicitly saves a
passwordless profile; no database connection is attempted:

```bash
make dev ARGS="db add --alias analytics --host localhost --database app --username reader --passwordless --yes"
make dev ARGS="db edit analytics --alias reporting --none --yes"
```

Use `--password-stdin` instead of `--passwordless` to read one UTF-8 password line
from standard input. Only one terminal LF or CRLF is removed; other whitespace is
preserved. `--credentials-stdin` accepts a strict JSON object with optional
`password`, `ssh_password`, `ssh_key_passphrase`, and `proxy_password` strings.
Both input modes require `--yes` and complete fields and cannot share stdin with
forms. Add requires an explicit password choice; omitted edit secrets remain
unchanged. No password argument or credential-bearing URL is accepted.

Advanced settings use `--tls`, `--tls-ca /absolute/path`,
`--ssh-host`, `--ssh-port`, `--ssh-user`, `--ssh-key-file`,
`--proxy socks5://host:port`, and `--proxy-user`. SSH and proxy are mutually
exclusive. Key files are imported once into the encrypted vault; their source
files remain untouched. `--tls=false`, `--clear-ssh`, and `--clear-proxy` remove
transport settings; the latter two also clear their secrets. Individual clear
flags are `--clear-ssh-password`, `--clear-ssh-key-passphrase`, and
`--clear-proxy-password`. TLS CA requires enabled TLS. The internal driver supports
password or imported-key SSH authentication (including encrypted keys and vault
passphrases), and authenticated or unauthenticated SOCKS5. TLS verifies the
original database hostname through either route. Database DNS resolution happens
at the jump host/proxy; only its endpoint is resolved locally. No SSH agent,
OpenSSH configuration, proxy environment or external helper is used.

Enroll an SSH host with interactive `db add ... --ssh-enroll` or
`db edit analytics --ssh-enroll`. Compare the displayed SHA-256 fingerprint with
the server's trusted fingerprint, confirm it, then confirm the profile preview.
Both confirmations default to No. Enrollment rejects `--yes`, stdin credential
flags and non-TTY input; scripted saving alone cannot trust a host. Pins publish
to the owned mode-0600 `config/known_hosts` before the profile; cancellation before
the final confirmation writes nothing. An interrupted save may leave an unused
pin. The file supports exact host/port public-key entries, without wildcards,
certificates or ambient known-host files. Unknown and changed keys fail at runtime;
changed keys are never automatically replaced. Normal saving makes no network
connection; `--ssh-enroll` only probes the SSH host key, without authentication or
database access. `db test` exercises the configured route.

`--query-timeout` accepts whole milliseconds from `1ms` to `30s`; `--max-rows`
accepts 1–5000 and `--max-result-bytes` accepts 1024–1048576. Defaults are `10s`,
500 and 1048576. Add/edit can replace scope with `--all`, `--none`, repeated
`--schema`/`--table schema.table`, or `--scope-json`. Structured JSON supports
identifiers containing dots. Exact selections require no catalog fetch; a new
connection defaults to all accessible tables, while an empty selection means
none. `db scope analytics --schema public --table other.Exact --yes` replaces the
whole scope without opening a database connection or accessing credentials.

`db scope [alias]` browses the role's full accessible application catalog,
including objects outside the saved scope. Up/Down moves, Right expands a schema,
Left returns to schemas, Space toggles, `/` searches the current level, `n` loads
the next page and `b` returns to its first page. Only 50 objects are fetched at a
time. Use `0` to start with none or `a` for all; broad selections cannot express
individual exclusions. Enter previews the exact selection and a default-No Y/N
confirmation saves it. Whole schemas include future tables; fixed table names do
not. Partition roots include their partitions, subject to driver support and
privileges. Names containing dots retain their exact identity. Renamed names are
unavailable; recreating a selected name selects the replacement object.

Failed browsing, cancellation and stale previews preserve saved scope. Saving
waits for existing database operations holding a state lease to finish. For a
manually written profile with no initialized installation state, run `db test`
or save with `db edit` before interactive browsing. Scripted scope replacement
works directly. Scope selection and catalog visibility never authorize otherwise
unsupported SQL.

`db test [alias]` checks one connection, or every saved connection in alias order.
It reports config, vault, dial (including TLS/SSH/proxy), authentication, version,
and policy stages. Policy includes the shared read-only role and semantic catalog
checks. Tests read no application rows and change no database data. Each profile
uses its timeout; profiles run sequentially and ordinary failures do not stop the
batch. JSON is `{"version":1,"results":[...]}`: each result has `alias`, `ok`, the
last reached `stage`, ordered `stages`, and a safe `error` on failure. Unreached
stages are omitted. Any failed profile produces a nonzero exit. An invalid whole
configuration or unknown alias exits 2 before producing a report; interruption
exits 130. Readiness does not authorize a query: every Query still compiles and
rechecks the current scope, relation identities and semantic catalog.

Manual profiles follow the same schema without an activation record. Malformed
configuration is preserved and reported. Editing a profile with a missing
credential bundle reports a repair instruction: supply its credentials through
`db edit` or explicitly clear them. Missing/corrupt vault keys or accounting are
not reset. A removal can durably save the profile change while credential cleanup
fails; the command exits 1 and reports that partial result. A subsequent confirmed
add/edit/remove retries orphan cleanup. Saving never starts services or registers
agents.

| Command | Behavior |
| --- | --- |
| `make help` | List all development commands; default target |
| `make setup` | Prepare pinned dependencies/tools without building the application |
| `make install` | Download pinned dependencies/tools and build the local binary |
| `make build` | Rebuild and atomically replace the local binary |
| `make dev ARGS="..."` | Run that binary with the checkout's absolute root |
| `make clean` | Alias for uninstall with purge disabled; not ready until P11, fails without changing files |
| `make format`, `make tidy` | Format Go and shell sources |
| `make format-check` | Check formatting without changing files |
| `make lint` | Run go vet and pinned staticcheck |
| `make test` | Run isolated unit/regression tests |
| `make test-race` | Run the suite with race detection |
| `make test-integration DB_DRIVER=postgres DB_IMAGE=postgres:16` | Run the owned PostgreSQL 16 Docker fixture; use `postgres:18` for 18 or omit `DB_IMAGE` for both; excluded from CI |
| `make uninstall`, `make uninstall PURGE=1` | Not ready until P11; fail without changing files |

CI runs `make setup` in each job before its checks; installation is exercised by
the regression suite and the Test and build job ends with `make build`.

The normal validation order is `make format-check`, `make lint`, `make test`,
`make test-race`, and `make build`. After installation, tests disable Go module
network access and use disposable roots. They do not connect to a real database,
read/write Keychain items, or alter agent configuration. The CI workflow uses
`macos-15`, records its actual architecture/toolchain, and runs these local gates.
A workflow file alone does not establish a successful hosted run.

The Docker suite requires Docker, OpenSSL and Python 3. It creates a random
network, volume, credentials and TLS certificates, publishes only an ephemeral
loopback port, and removes its resources on success, failure and interruption.
It accepts only `DB_DRIVER=postgres` and the two documented images, refuses
external endpoint variables and CI, and records image digests/server versions.
It never connects to a supplied database URL. Images remain in Docker's cache.

P3 checks actual authenticated identity, PostgreSQL 16+, CONNECT, inherited and
SET-reachable role privileges, and PUBLIC grants on every operation. Use a
non-owner role without elevated, creation, relation/column write, sequence write,
or server file/program privileges. These safety checks cover application objects
throughout the database, even outside the saved scope; TEMP alone is allowed.
The driver does not modify grants. Startup discards inherited `PG*` settings;
password/service files, ambient TLS certificates and connection fallbacks are not
used. TLS verifies the configured database hostname and never falls back.

Metadata applies exact saved scope, schema USAGE and table SELECT, hides foreign
keys to unavailable targets, and omits expressions/defaults. Pages use signed
process-local cursors invalidated by profile/scope changes, explicit invalidation,
or restart. Supported query candidates are ordinary/partitioned heap tables with
supported built-in types; views, foreign/materialized relations, custom types,
generated columns and unverified relation features are reported as unsupported.
Partition roots include their tree across schemas; ordinary inheritance requires
all descendants in scope. RLS with inheritance/partitioning is unsupported;
ordinary noninherited RLS remains supported. The compiler freezes physical scans
with ONLY/UNION ALL. Execution locks captured relations in OID order, then checks
names/OIDs, all column layouts, hierarchy edges, scope and privileges again in a
read-only READ COMMITTED transaction. A changed capture fails with retry guidance;
queries are never automatically replayed. A partition attached after the final
check enters only subsequent requests. Even a childless ordinary table uses
ONLY, so a newly inherited child cannot enter an authorized scan. PostgreSQL
16/18 fixtures exercise these DDL boundaries. Catalog support status alone does
not authorize a query.

The [SQL compiler contract](internal/database/postgres/sqlpolicy/README.md) lists
accepted forms and conservative exclusions. It supports joins, filters, grouping,
audited aggregates, CTEs, subqueries, typed parameters and bounded pagination.
Custom types/functions/operators/collations, unsupported indexes and unhandled
syntax fail closed. Literals are bound separately with exact built-in type OIDs;
submitted SQL is never prepared during compilation. Catalog signatures are
pinned separately for PostgreSQL 16 and 18; other majors remain unavailable for
compilation until audited. Verification ignores incidental catalog row IDs and
numeric planner estimates while retaining implementation and safety properties.
Only embedded policy is cached; live metadata is checked during compilation
and again after locking, including queries without base relations.
The compiler contract documents candidate export through the owned integration
harness. The executor is an internal driver API; CLI and MCP query delivery are
not exposed in this package.

Queries use extended-protocol text results with explicit built-in OIDs. The
compiler lowers the top-level LIMIT to the row cap plus one lookahead row while
preserving smaller and nested limits. Results preserve duplicate column labels
and contain complete rows only. Byte accounting includes JSON escaping, base64,
column metadata, envelope fields and a duplicated compatibility text block.
Oversized rows are omitted with `truncated:true`; metadata that cannot fit and
protocol bodies over 2 MiB fail with `RESOURCE_LIMIT`. Byte truncation terminates
the connection instead of draining remaining rows. Completed reads roll back;
timeout/cancellation returns no partial result, and uncertain connections are
discarded. Results are fully prepared before driver admission/state access ends.

| PostgreSQL types | Result representation |
| --- | --- |
| bool, int2/int4 | JSON boolean or integer |
| int8/numeric | Exact decimal strings; numeric special values remain strings |
| float4/float8 | Finite JSON numbers; `NaN`, `Infinity`, `-Infinity` as strings |
| text/varchar/bpchar/uuid | Strings; bpchar padding is preserved |
| date/time/timestamp | ISO local strings; timestamps have no invented timezone |
| timestamptz | UTC ISO strings ending in `Z` |
| bytea | Base64 strings with the column encoding marker `base64` |
| json/jsonb | Structured JSON with exact numbers; bounded to 1 MiB and depth 64 |
| SQL NULL | JSON null for every type |

Temporal infinity and BC values remain explicit strings, for example
`infinity` and `0001-01-01T00:00:00Z BC`. Typed parameters use these JSON
representations and canonical type names, with at most 256 parameters, 64 KiB
per value and 256 KiB total; converted wire values obey the compiler's same byte
limits. Temporal parameters require ISO forms; timestamptz may carry an explicit
ISO offset. PostgreSQL validates calendar/range constraints through audited
built-in input functions. Errors never include SQL or parameter values.

Pools are lazy (two connections each, sixteen pools, five-minute idle eviction),
with eight active operations, thirty-two waiters and a five-second queue deadline
per shared driver. Requests have profile timeouts, a two-MiB protocol body cap,
bounded catalog results and redacted errors. No application rows are queried by
connection tests or catalog operations. Service state-lease integration is P8.

P1's explicit native check uses only synthetic credentials, a random temporary
root, one exact Keychain account, and a temporary launchd job:

```bash
DATA_MATE_NATIVE_TEST=1 make test
```

This opt-in requires an unlocked macOS user Keychain and a GUI launchd session.
The default suite skips this native gate and uses fake providers. The native
fixture also creates its own temporary Keychain for locked/denied/unavailable
checks; it never locks the user's Keychain. A rebuilt ad-hoc executable may need
new OS authorization: denial preserves the vault and never resets its key.
Unattended access reports denial rather than displaying UI. If fixture cleanup
fails, the test reports and retains its exact helper/root for retry.

Vault writes reserve one of a maximum of 1,000,000 encryptions durably before
sealing. Failed publication consumes its reservation. Profile edits publish a
fresh encrypted bundle before its reference; deletion publishes profiles before
credential cleanup and reports partial completion if cleanup fails. The internal
purge coordinator removes the exact key before its accounting, retains retry
identity, and blocks ordinary access once purge begins. Full `uninstall`/`clean`
integration remains P11. Do not restore old vault/accounting backups or move
Keychain items between stores; recovery is nonsecret profile import and credential
re-entry in a fresh installation namespace.

Scripts under `scripts/` back the matching Make targets; `common.sh` supplies
checkout paths, target settings, dependency checks, and directory validation.
`install.sh` runs `setup.sh` before `build.sh`.
The `clean` target delegates to `uninstall` with `PURGE=0`, overriding any supplied
`PURGE` value. Use `make uninstall PURGE=1` to explicitly request purge.
`dev` invokes the binary directly and `test-race` adds `-race` to `test.sh`.

Profile and MCP contracts are embedded in `internal/contracts/schemas`, with
examples in `internal/contracts/testdata` and a decoder fixture in
`internal/config/testdata`. Profiles reject unknown fields,
duplicate keys/identities/references, malformed scopes, unsupported transports,
and excess limits. No plaintext secret field belongs in a profile. Selected
empty scopes expose nothing; missing scope is invalid. New profiles explicitly
choose all. PostgreSQL codec/signature fixture formats are
under `internal/database/postgres/testdata` and execute in offline and PostgreSQL
16/18 regressions. The compiler's reviewed per-major semantic manifests live under
`internal/database/postgres/sqlpolicy`.

See [PRD](specs/PRD.md) and [technical design](specs/DESIGN.md) for intended product
behavior. Local implementation progress and evidence live in ignored `.misc`.
