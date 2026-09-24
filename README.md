# Data Mate

A checkout-local Go CLI for sharing PostgreSQL connections with terminal agents
through a read-only MCP service on macOS with Apple Silicon.

**Local PostgreSQL profiles, read-only MCP, and owned cleanup are implemented.**
`db add`, `db edit`,
`db remove`/`rm`, `db list`/`ls`, `db scope`, and `db test` use the profile/vault store. Interactive
forms, scripted input, strict profiles, atomic publication, AES-256-GCM, durable
encryption accounting, and one native Keychain item per installation are
implemented. The internal PostgreSQL driver supports direct, SSH and SOCKS5
routes with optional verified TLS,
read-only transactions, scoped catalog pages and table descriptions. A small
query guard checks statement kind and direct table/view scope; PostgreSQL handles
SQL semantics and permissions. Arrays, enums, views, custom types, generated
columns and normal PostgreSQL read queries are supported. `mcp start`, `mcp stop` and `mcp status` manage
the background service. The stdio bridge exposes four read-only MCP tools through
the shared driver. Start also registers the bridge with installed Codex and
Claude Code clients; new sessions can discover it from other working directories.
`make uninstall` removes the service, owned registrations and executable while
preserving profiles and credentials; `PURGE=1` explicitly removes those too.
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
Each checkout has its own `.dev`. Install/build initialize its private `bin`,
`config` and `state` directories, installation identity, empty profiles and stable
locks, then publish the executable atomically under the lifecycle lock. Existing
profiles and credentials are preserved. No service, agent registration or
Keychain item is created by installation. Listing, status, previews and canceled
forms do not initialize state. Builds report a required restart when service
state exists; run `mcp stop` then `mcp start` to load the new executable.

Manage the background service:

```bash
make dev ARGS="mcp start"
make dev ARGS="mcp status --json"
make dev ARGS="mcp stop"
```

These commands support `--json` with version 1, a service `state`, and per-agent
states. Agent states are `unavailable`, `pending`, `ready`, `disabled`, `conflict`
and `failed`. Start output names the root and `data-mate-dev-<root-hash>`
registration. After service readiness, start adds missing user-scope entries using
supported client commands and verifies the result. Absent clients are skipped;
a failed adapter returns exit 1 with a structured partial report while the
healthy service and successful adapter remain available. Retry `mcp start` after
resolving the reported agent state. Stop preserves registrations.
Start explicitly bootstraps one job in the current user's GUI launchd
domain and waits up to 30 seconds for readiness. There is no login item or
automatic restart after a crash. Repeated starts reuse the matching healthy job.
Stop preserves profiles and credentials, cancels active work, and allows five
seconds before launchd terminates the verified job. It never signals a PID merely
because it appears in a file.

Startup validates profiles and existing encrypted credentials. It retains one
vault key in process memory; an empty installation neither loads nor creates a
key. Startup and status open no database connections, so an unreachable database
does not prevent service readiness. Native Keychain denial or timeout fails
startup and cleans the partial job. A rebuilt executable may need fresh native
approval. Status is passive: no secret access, service startup, database access,
agent command or repair. States are `stopped`, `starting`, `running`, `degraded`
and `stale`. Stopped status exits 0; invalid configuration/state exits 2 and other
inspection failures exit 1. A status failure still emits its JSON report when
requested, with diagnostics on stderr.

Each internal operation validates fresh profile bytes after bounded admission and
holds a shared state lease through driver cleanup and result preparation. Profile
changes retire the affected pools, transports and cursors; invalid configuration
blocks work until repaired. CLI mutations wait for old-snapshot work to finish.
The service uses the same driver and request-local catalog verification as the
CLI. Every MCP database call uses this path; successful query authorization is
never cached. Slow socket output does not hold a state lease.

The owned runtime socket uses a short private temporary path, full installation
identity, a per-start nonce and peer UID/PID checks. Build or protocol mismatch
requires an explicit restart; bridges never start a stopped service. Foreign
jobs, mismatched records and unsafe files are preserved and reported as conflicts.
Service roots may contain spaces and long directory names, but not control
characters. Unknown launchctl inspection formats fail closed.

The internal `mcp bridge` command connects to an already running service and
relays newline-delimited MCP on stdin/stdout. It uses the executable-relative root
or an explicit `--root`; startup diagnostics go to stderr. Bridge stdout must be
a pipe or socket so blocked writes can be interrupted. The bridge exposes:

| Tool | Inputs | Result |
| --- | --- | --- |
| `list_connections` | Empty object | Aliases, drivers, database labels and saved scope |
| `list_tables` | `connection`; optional `schema`, `cursor`, `page_size` | Scoped table page with authenticated next cursor |
| `describe_table` | `connection`, `schema`, `table` | Columns, keys and visible relationships |
| `query` | `connection`, `sql`; optional JSON-value `parameters`, `row_limit` | Bounded columns and rows, truncation and elapsed time |

Inputs reject unknown fields. Tools cannot supply credentials or override scope.
Results provide identical structured data and compact JSON text, with a combined
one-MiB budget (or the lower profile cap). Expected failures carry a safe code,
message and retry flag. Cursors expire on restart or relevant profile changes.

Sessions negotiate MCP `2025-11-25` with the pinned official SDK. Newer SDK clients
fall back from stateless discovery to initialization. The service allows sixteen
sessions and four in-flight requests per session, with 256-KiB inbound and two-MiB
outbound frame limits. Excess session/dispatch work or invalid framing closes the
peer; database queue exhaustion returns a safe tool error. Initialization has ten
seconds; blocked output has five. Cancellation and disconnect release database
work. A bridge never starts the service.

Agent detection uses absolute PATH directories at CLI invocation. Codex reads
`$CODEX_HOME/config.toml` (default `~/.codex/config.toml`); Claude reads
`$CLAUDE_CONFIG_DIR/.claude.json` (default `~/.claude.json`). Relative overrides,
unsafe files and unknown registration layouts fail without repair. Passive
status reads these files directly, capped at 4 MiB; it never invokes client
health checks. Registration subprocesses have eight-second and 64-KiB output
bounds, and their output is not exposed in diagnostics.

Each checkout records an add intent before invoking the client. A retry can
reconcile an interrupted add when the exact command, arguments and root match.
Conflicts are preserved. Matching manually created entries are usable but are
not adopted for deletion; cleanup requires recorded ownership and an unchanged
fingerprint. Use the same client configuration location for start and cleanup.
Data Mate preserves disabled-server, trust, approval, sandbox and project
settings. `ready` describes the user registration; native policy or a project
override may still prevent a session from using it. Codex entries with
`enabled = false` report `disabled`. New sessions load the registration; existing
sessions may need their normal reconnect or restart action.

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

New connections use direct plaintext PostgreSQL transport by default. Enable
`--tls` to require hostname-verified TLS; SSH and SOCKS5 are opt-in routes.

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

`--query-timeout` accepts whole milliseconds from `1ms` to `5m`; `--max-rows`
accepts 1–5000 and `--max-result-bytes` accepts 1024–1048576. Defaults are `60s`,
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
not. Partition roots include their partitions, subject to PostgreSQL
privileges. Names containing dots retain their exact identity. Renamed names are
unavailable; recreating a selected name selects the replacement object.

Failed browsing, cancellation and stale previews preserve saved scope. Saving
waits for existing database operations holding a state lease to finish. For a
manually written profile with no initialized installation state, run `db test`
or save with `db edit` before interactive browsing. Scripted scope replacement
works directly. Scope applies to direct relation references; it does not recursively inspect
view/function dependencies.

`db test [alias]` checks one connection, or every saved connection in alias order.
It reports config, vault, dial (including TLS/SSH/proxy), authentication, version,
and read_only stages. The final stage verifies an actual read-only transaction
and the authenticated identity, without auditing account grants. Tests read no application rows and change no database data. Each profile
uses its timeout; profiles run sequentially and ordinary failures do not stop the
batch. JSON is `{"version":1,"results":[...]}`: each result has `alias`, `ok`, the
last reached `stage`, ordered `stages`, and a safe `error` on failure. Unreached
stages are omitted. Any failed profile produces a nonzero exit. An invalid whole
configuration or unknown alias exits 2 before producing a report; interruption
exits 130. Readiness does not certify account grants. Every query checks the current direct
scope and runs in a read-only transaction.

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
| `make clean` | Uninstall with purge disabled, even when `PURGE=1` is supplied |
| `make format`, `make tidy` | Format Go and shell sources |
| `make format-check` | Check formatting without changing files |
| `make lint` | Run go vet and pinned staticcheck |
| `make test` | Run isolated unit/regression tests |
| `make test-race` | Run the suite with race detection |
| `make test-integration DB_DRIVER=postgres DB_IMAGE=postgres:16` | Run the owned PostgreSQL 16 Docker fixture; use `postgres:18` for 18 or omit `DB_IMAGE` for both; excluded from CI |
| `make uninstall` | Stop service; remove owned registrations and binary; preserve profiles, vault and key |
| `make uninstall PURGE=1` | Additionally delete the exact Keychain item and owned profiles, vault, pins and state |

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

Use an operator-provisioned, non-owner read-only account with CONNECT, schema
USAGE and SELECT on intended tables or columns. Data Mate does not modify or
audit grants. Every operation runs in an explicit read-only transaction, even
if the account has a different default. PostgreSQL 16+ is accepted; 16 and 18
are the integration matrix. SQL syntax follows the pinned PostgreSQL parser.

The configured scope covers directly named application tables/views. Use
`schema.table`; CTE names remain unqualified. `all` includes accessible application
relations, while `pg_*` schemas and `information_schema` remain excluded. Views,
materialized views, foreign tables, partitions, inheritance, generated columns,
RLS and custom types use PostgreSQL's normal behavior. View/function dependencies
are governed by database permissions, not recursively filtered by Data Mate.
Database-installed routines and extensions are trusted; read-only transactions
are not a sandbox for arbitrary code or external effects.

Metadata includes relations readable through table or column SELECT grants,
filters foreign-key endpoints by scope, and reports actual type names without
support flags. Each page uses a single relation query, without per-table
inspection. Signed cursors expire on profile changes, invalidation or restart.

Queries accept one SELECT-family statement, including VALUES, TABLE, recursive
CTEs, joins, windows, correlated/lateral queries, set operations, functions and
casts. The guard rejects writes, modifying CTEs, SELECT INTO, locking clauses,
multiple statements and transaction/session/utility commands. PostgreSQL receives
the original SQL and separately bound values; there is no semantic compiler,
SQL rewriting, catalog fingerprinting or index/role inspection.

For example, the MCP query input is:

```json
{
  "connection": "analytics",
  "sql": "SELECT * FROM public.orders WHERE id = $1::uuid",
  "parameters": ["550e8400-e29b-41d4-a716-446655440000"]
}
```

PostgreSQL infers parameter types or uses explicit SQL casts. JSON strings become
their contents, numbers retain exact decimal text, booleans become text and null
becomes SQL NULL. Objects/arrays become compact JSON text for casts such as
`$1::jsonb`; PostgreSQL arrays use array-text strings such as `"{a,b}"` with
`$1::text[]`. To pass JSON null, use the string `"null"` and a JSON cast. The limit
is 256 parameters, 64 KiB per value and 256 KiB total. Values are never inserted
into SQL text.

| PostgreSQL types | Result representation |
| --- | --- |
| bool, int2/int4 | JSON boolean or integer |
| int8/numeric | Exact strings |
| float4/float8 | JSON numbers; special values as strings |
| text/varchar/bpchar/uuid | Strings |
| date/time/timestamp | Local strings; no invented timezone |
| timestamptz | UTC ISO strings ending in `Z` |
| bytea | Base64 with `encoding: base64` |
| json/jsonb | Structured JSON preserving exact numbers |
| arrays | Nested JSON arrays, preserving null elements and shape |
| domains | Base-type representation |
| enums, ranges, composites, other types | PostgreSQL text with actual type names and `encoding: postgres_text` |
| SQL NULL | JSON null |

Array lower bounds normalize to JSON indexing; array encoding markers describe
elements. Empty arrays are `[]`. Temporal infinity and BC values remain explicit
strings. An unfamiliar unselected column never blocks a query.

Results preserve duplicate labels and contain complete rows only. Row/byte
truncation closes the connection instead of draining the rest of the query.
Successful operations roll back and reset the session before pool reuse;
timeouts/cancellation return no partial result. Limits include column metadata,
JSON escaping, base64 and both MCP result representations. Oversized metadata
or protocol messages fail with `RESOURCE_LIMIT`.

Failures distinguish `READ_ONLY_VIOLATION`, `PERMISSION_DENIED`, invalid SQL or
parameters, query failures, connection failures and timeouts. PostgreSQL errors
include a safe SQLSTATE but never upstream details, SQL or parameter values.
Startup discards inherited `PG*` settings; explicit transport configuration and
credential protection remain unchanged.

Pools are lazy (eight connections each, sixteen pools, five-minute idle eviction),
with 32 active operations, 128 waiters and a 60-second queue deadline
per shared driver. Requests have profile timeouts, a two-MiB protocol body cap,
bounded catalog results and redacted errors. No application rows are queried by
connection tests or catalog operations. The service manager uses the same state
leases and shared driver for all MCP sessions. Each MCP session admits at most
16 concurrent requests, including control requests; excess requests close that
session. Up to 16 sessions/handshakes and 16 pools are supported, allowing at most
128 pooled database connections.

The profile timeout defaults to 60 seconds and can be raised to five minutes with
`db edit <alias> --query-timeout 5m`. After service admission and profile resolution,
credentials, driver admission, pool waiting, setup, execution and result reading
share that budget. The service has a 365-second outer deadline; earlier caller
deadlines still apply. Connection setup remains capped at ten seconds and lock
waits at one second. Lock timeouts also return `QUERY_TIMEOUT`.

Actual authenticated Codex/Claude workflows are a separate opt-in:

```bash
DATA_MATE_AGENT_TEST=1 make test-integration DB_DRIVER=postgres DB_IMAGE=postgres:16
```

This uses an owned Docker database, temporary installation and unique registration
names in the clients' current user configuration. It requires authenticated
clients, an unlocked Keychain and GUI launchd, may use normal model quota, and
removes only the fixture's registrations, service and key afterward. Cleanup
failure retains the exact retry root. Codex uses its configured permissions;
Claude uses manual mode with a normal allow rule limited to the fixture's four
read-only tools, plus an explicit query-deny check. No bypass permission mode is
used. The default suite never reads user agent configuration or authentication.
Native service-only checks use isolated client configuration directories.

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
identity, and blocks ordinary access once purge begins. Uninstall holds lifecycle
through service shutdown, registration cleanup and exclusive state access.
Do not restore old vault/accounting backups or move
Keychain items between stores; recovery is nonsecret profile import and credential
re-entry in a fresh installation namespace.

Scripts under `scripts/` back the matching Make targets; `common.sh` supplies
checkout paths, target settings, dependency checks, and directory validation.
`install.sh` runs `setup.sh` before `build.sh`.
The `clean` target delegates to `uninstall` with `PURGE=0`, overriding any supplied
`PURGE` value. Use `make uninstall PURGE=1` to explicitly request purge.
`dev` invokes the binary directly and `test-race` adds `-race` to `test.sh`.

Uninstall builds a temporary helper under `/tmp` using the same Go/C toolchain and
installed modules. It works after the installed executable has been removed and
from a checkout containing spaces. Success removes the helper; failure retains
it and prints the exact retry command, cleanup stage and remaining owned files.
Use the same `CODEX_HOME`/`CLAUDE_CONFIG_DIR` as when starting the service. If an
owned agent entry was edited or its client is unavailable, cleanup preserves the
entry and executable for repair/retry; it never overwrites client settings.
Unexpected files in the private runtime socket directory also stop cleanup so
its ownership marker remains available; move those files aside before retrying.

Default uninstall keeps the installation identity, profiles, encrypted vault,
usage ledger, SSH pins and Keychain item. `make install` restores the executable;
`mcp start` validates the retained credentials and restores registrations. Native
approval may be required after a rebuild. Purge first stops the service and
removes verified registrations, then marks a durable tombstone and deletes the
exact key before its vault/accounting. A pending purge must be retried with
`PURGE=1`; rebuilding cannot reactivate it. A final nonsecret receipt supports
retry across identity/lock removal. Only inventoried files and empty directories
are removed. Unrelated `.dev` files, external certificates and imported key
source files remain intact. No persistent service logs
are created. Go's shared caches and unrelated build files are not installation
artifacts and are preserved.

The complete [profile JSON schema](internal/contracts/schemas/profiles.json) and
[example profile](internal/config/testdata/profiles.json) define manual editing:

| Field | Contract |
| --- | --- |
| Document | `version:1`, `connections` array; maximum 1 MiB |
| Profile | UUID `id`, unique lowercase `alias`, `driver:"postgres"`, `connection`, `transport`, `scope`; optional vault `credential_ref` and `limits` |
| `connection` | Nonempty `host`, `database`, `username`; integer `port` 1–65535 |
| `transport.tls` | `mode:"disabled"` or `"verify-full"`; optional absolute `ca_file` for verified TLS |
| `transport.ssh` | Optional `host`, `port`, `user`, `auth:"password"` or `"key"`; secret key material stays in the vault |
| `transport.proxy` | Optional `kind:"socks5"`, `host`, `port`, optional `username`; mutually exclusive with SSH |
| `scope` | `mode:"all"`, or `"selected"` with optional `schemas` and `tables:[{"schema":"public","name":"example"}]`; empty selection exposes nothing |
| `limits` | Optional `query_timeout_ms` (1–300000), `max_rows` (1–5000), `max_result_bytes` (1024–1048576); omitted values default to 60000, 500, 1048576 |

Profile and MCP contracts are embedded in `internal/contracts/schemas`, with
examples in `internal/contracts/testdata` and a decoder fixture in
`internal/config/testdata`. Profiles reject unknown fields,
duplicate keys/identities/references, malformed scopes, unsupported transports,
and excess limits. No plaintext secret field belongs in a profile. Selected
empty scopes permit no direct relations; missing scope is invalid. New profiles explicitly
choose all. PostgreSQL codec fixture formats are
under `internal/database/postgres/testdata` and execute in offline and PostgreSQL
16/18 regressions.

See [PRD](specs/PRD.md) and [technical design](specs/DESIGN.md) for intended product
behavior. Local implementation progress and evidence live in ignored `.misc`.
