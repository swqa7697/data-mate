# Data Mate

**Read-only PostgreSQL access for terminal coding agents, managed from one local CLI.**

Data Mate saves database connections in a checkout-local installation and exposes them through a background [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) service. Codex and Claude Code connect to the service through registered stdio bridges. Agents can discover tables and run bounded read queries without receiving database credentials.

Data Mate currently supports macOS on Apple Silicon and PostgreSQL 16 or later. The PostgreSQL 16 and 18 releases are covered by the optional integration test matrix. Installation is for development in this checkout's `.dev` directory; there is no distributed package or automatic updater.

## Requirements

| Requirement              | Version or purpose                                                   |
| ------------------------ | -------------------------------------------------------------------- |
| macOS on Apple Silicon   | Supported host platform; service management uses user launchd        |
| Go                       | 1.27.1 toolchain, pinned in `go.mod` and the build scripts           |
| Xcode Command Line Tools | `xcrun`, Clang, and the macOS SDK for native builds                  |
| PostgreSQL               | Server version 16 or later and an operator-managed read-only account |
| Codex or Claude Code     | Install at least one to use Data Mate from an agent                  |
| Docker                   | Optional; required only for PostgreSQL integration tests             |

## Quick start

From the repository root:

```bash
make install
make dev ARGS="db add"
make dev ARGS="db test analytics"
make dev ARGS="mcp start"
make dev ARGS="mcp status"
```

`db add` opens an interactive form for the alias, host, port, database, username, and hidden password. Replace `analytics` with the alias you chose. A new connection exposes all accessible application tables by default. To narrow it before enabling MCP, run `make dev ARGS="db scope analytics"` and select schemas or tables.

Confirmed connection changes, `db test`, and interactive scope browsing start or reuse a persistent management-only service. Listing and previews remain passive. `mcp start` enables MCP on that service and registers it with installed supported agents. Launch a new Codex or Claude Code session normally; existing sessions may need a reconnect or restart to load the registration. The service can start without a reachable database because connections open when requested. Use `db test` to check database connectivity and read-only transaction readiness.

`mcp status --json` reports `mcp_enabled` and `keyset_state` separately from process state. A management-only process is unavailable to agents.

Stop the service with `make dev ARGS="mcp stop"`. Rebuilding a running installation with `make build` requires a subsequent `mcp stop` and `mcp start` to use the new executable.

## Connections and scope

| Command                                 | Purpose                                                                                  |
| --------------------------------------- | ---------------------------------------------------------------------------------------- |
| `db add`                                | Save a PostgreSQL connection; interactive unless complete flags and `--yes` are supplied |
| `db edit [alias]`                       | Change nonsecret settings or credentials; select a connection if the alias is omitted    |
| `db scope [alias]`                      | Choose visible schemas and tables interactively or with scope flags                      |
| `db list` / `db ls`                     | List saved nonsecret connection settings and scope                                       |
| `db test [alias]`                       | Check one connection, or all connections when the alias is omitted                       |
| `db remove [alias]` / `db rm [alias]`   | Remove a saved connection and its managed credentials                                    |
| `mcp start` / `mcp stop` / `mcp status` | Manage and inspect the background service                                                |
| `help` / `version`                      | Show command help or build information                                                   |

All examples use `make dev ARGS="..."`, which runs `.dev/bin/data-mate` with this checkout's installation root. Run `make dev ARGS="db add --help"` for transport, limit, credential, and scope flags. `db list`, `db test`, `mcp start`, `mcp stop`, and `mcp status` accept `--json` for versioned machine output.

For scripts, supply complete connection fields and `--yes`; use `--password-stdin` for one UTF-8 password line or `--credentials-stdin` for a strict JSON credential object. Data Mate does not accept secret values as command-line arguments. A passwordless connection requires explicit `--passwordless`.

Scope can be set during `db add` or `db edit`, or replaced later with `db scope`. For example:

```bash
make dev ARGS="db scope analytics --schema reporting --table public.orders --yes"
make dev ARGS="db list --json"
```

`--schema` selects all current and future tables in that schema; `--table` selects one exact `schema.table` name. `--all` restores all accessible application tables, and `--none` selects no direct relations. Saved scope limits direct relation references and catalog results; PostgreSQL privileges and database-defined views, functions, and row-level security still apply. Use a dedicated non-owner account with only the required `CONNECT`, schema `USAGE`, and `SELECT` grants.

Direct TCP is the default. Optional verified TLS, an SSH jump host, or a SOCKS5 proxy can be configured with `db add` or `db edit`. SSH host fingerprint enrollment is interactive. See [the technical design](specs/DESIGN.md) for transport and scope details.

## Agent tools

The MCP service provides four tools:

| Tool               | Result                                                       |
| ------------------ | ------------------------------------------------------------ |
| `list_connections` | Available aliases, database labels, and saved scopes         |
| `list_tables`      | Visible relations, with pagination                           |
| `describe_table`   | Columns, types, keys, and visible relationships              |
| `query`            | One read statement with bound parameters and bounded results |

Queries run in read-only transactions. The query guard permits one `SELECT`-family statement and checks directly referenced relations against saved scope. By default, a profile allows a 60-second query budget, 500 returned rows, and a 1 MiB result limit; the query timeout can be configured up to five minutes. Data Mate stores nonsecret profiles and separate Tink-encrypted credential bundles in `.dev/state/data-mate.db`. One serialized AES-256-GCM Tink keyset per installation lives in the macOS Keychain. The service retains the loaded keyset until stopped; ordinary connection additions and edits then require no further Keychain access. Initialization and unlocking after restart may prompt, especially after an unsigned development rebuild. Noninteractive callers receive an unlock-required error when OS interaction is needed. Scope-only changes and deletion need no unlock. No MCP tool accepts credentials or changes saved connections.

## Development

| Make target                                | Purpose                                                                       |
| ------------------------------------------ | ----------------------------------------------------------------------------- |
| `make help`                                | List developer commands                                                       |
| `make setup`                               | Download pinned Go dependencies and tools without building                    |
| `make install`                             | Set up dependencies and install `.dev/bin/data-mate`                          |
| `make build`                               | Rebuild the installed executable                                              |
| `make dev ARGS="help"`                     | Run the installed CLI                                                         |
| `make format` / `make tidy`                | Format Go and Bash source                                                     |
| `make format-check`                        | Check formatting without changing files                                       |
| `make lint`                                | Run `go vet` and pinned `staticcheck`                                         |
| `make test` / `make test-race`             | Run isolated offline regressions, optionally with the race detector           |
| `make test-integration DB_DRIVER=postgres` | Run owned Docker fixtures for PostgreSQL 16 and 18                            |
| `make clean` / `make uninstall`            | Remove owned installation resources while preserving SQLite profiles and credentials |
| `make uninstall PURGE=1`                   | Also delete owned profiles, credentials, host pins, and the installation keyset  |

To run only one PostgreSQL integration version, add `DB_IMAGE=postgres:16` or `DB_IMAGE=postgres:18`. Docker tests require a running Docker daemon and are excluded from CI. Native Keychain and launchd checks require an unlocked macOS user session and explicit opt-in:

```bash
DATA_MATE_NATIVE_TEST=1 go test -mod=readonly -run '^TestNativeKeychainLifecycle$' ./internal/vault
DATA_MATE_NATIVE_TEST=1 go test -mod=readonly -run '^TestNativeServiceLifecycle$' ./internal/service
```

Source layout: `cmd/data-mate` is the executable entry point; `internal/cli` owns user commands; `internal/config` and `internal/vault` own profiles and credentials; `internal/database/postgres` and `internal/transport` own database access; `internal/service`, `internal/mcp`, and `internal/agent` own the service and agent integration. See [AGENTS.md](AGENTS.md) for contribution and validation rules, [PRD](specs/PRD.md) for product scope, and [DESIGN](specs/DESIGN.md) for technical contracts.

## Development state compatibility

The development-only `connections.json`, `vault.json`, and `vault-usage.json` formats are obsolete. Data Mate reports them without converting or deleting them. Before rebuilding an older installation, use its original binary and cleanup helper to stop/uninstall it and, only if you intend to discard its saved credentials, purge its verified state. Preserve unrelated files. Recreate connections and re-enter credentials in the new installation; copying SQLite does not transfer its Keychain keyset or installation identity.

If the original cleanup tools are unavailable, preserve the old state and use a fresh checkout installation. Do not delete broad Keychain matches or assume that filenames alone establish ownership.

## Troubleshooting

- **SQLite recovery required:** Run an explicit management operation to let SQLite recover its owned journal. Passive listing/status do not start a writer; do not remove a hot journal manually.
- **Keyset unavailable:** Unlock through an explicit interactive operation. A missing ready keyset or mismatched metadata requires repair; Data Mate preserves ciphertext and never creates a substitute keyset.
- **Agent does not see Data Mate:** Run `make dev ARGS="mcp status"`. Start the service if stopped, then open a new agent session. Missing clients are skipped during registration.
- **Database test fails:** Check its reported stage, connection settings, transport, server availability, credentials, and account grants. A successful test verifies a read-only transaction, not the account's full privilege policy.
- **Build tools are missing:** Run `make setup` for dependency diagnostics. Install Go 1.27.1 and the Xcode Command Line Tools if prompted.
- **Rebuilt binary needs a restart:** Run `make dev ARGS="mcp stop"` followed by `make dev ARGS="mcp start"`.

## License

[GNU General Public License v3](LICENSE).
