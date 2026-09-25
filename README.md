# Data Mate

**Read-only PostgreSQL access for terminal coding agents, managed from one local CLI.**

Data Mate saves database connections in a user-owned installation and exposes them through a background [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) service. Codex and Claude Code connect to the service through registered stdio bridges. Agents can discover tables and run bounded read queries without receiving database credentials.

Data Mate currently supports macOS on Apple Silicon and PostgreSQL 16 or later. The PostgreSQL 16 and 18 releases are covered by the optional integration test matrix. Development uses this checkout's `.dev` directory. Production targets macOS 15 or later on Apple Silicon and uses a signed, notarized standalone executable.

Production installation, upgrades, cleanup, and Bash/zsh completion are implemented. The release bootstrap URL becomes available only after the first stable release is published; clean macOS 15 acceptance and hosted release credentials remain release gates. See the [distribution contract](specs/DESIGN.md#13-production-distribution-and-terminal-integration).

## Production installation

After the first stable release is published:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://github.com/swqa7697/data-mate/releases/latest/download/install.sh | /bin/bash
```

The installer refuses root, verifies the exact release's signature and notarization, and installs `~/.local/share/data-mate/bin/data-mate` with a command symlink at `~/.local/bin/data-mate`. Production resolves these paths from the OS account home and rejects every `--root` override. Installation does not start the service or initialize a credential keyset.

The installer configures the account's supported login shell (Bash or zsh). Open a new terminal or run the printed `source` command. To leave startup files unchanged, pass `-s -- --no-shell` to `/bin/bash` in the installation command. `data-mate completion bash` and `data-mate completion zsh` also generate scripts without installing anything. Alias completion reads only bounded nonsecret state.

```bash
data-mate db add
data-mate db test analytics
data-mate mcp start
data-mate upgrade                 # update is an alias
data-mate uninstall               # retain saved connections and credentials
data-mate uninstall --purge       # remove all owned data and the exact keyset
```

Uninstall previews its recorded scope and defaults to No; scripts must supply `--yes`. Upgrade reuses the installer engine, rejects downgrades/incompatible stores, and leaves the production service stopped. Run `data-mate mcp start` to resume it. Interrupted publication or cleanup retains ownership records and prints recovery guidance. After default uninstall, the bootstrap can purge retained data with `/bin/bash -s -- --uninstall --purge`.

Only one service may run per macOS user, including management-only services across development and production. A competing installation reports the owner and its explicit stop command; it never uses that owner's profiles or credentials. Status JSON version 2 includes an optional `blocking_owner` separately from the selected installation's state.

## Development requirements

| Requirement              | Version or purpose                                                   |
| ------------------------ | -------------------------------------------------------------------- |
| macOS on Apple Silicon   | Supported host platform; service management uses user launchd        |
| Go                       | 1.27.1 toolchain, pinned in `go.mod` and the build scripts           |
| Xcode Command Line Tools | `xcrun`, Clang, and the macOS SDK for native builds                  |
| PostgreSQL               | Server version 16 or later and an operator-managed read-only account |
| Codex or Claude Code     | Install at least one to use Data Mate from an agent                  |
| Docker                   | Optional; required only for PostgreSQL integration tests             |

## Development quick start

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

Development examples use `make dev ARGS="..."`, which runs `.dev/bin/data-mate` with `--root <checkout>/.dev/data-mate`. Run `make dev ARGS="db add --help"` for transport, limit, credential, and scope flags. `db list`, `db test`, `mcp start`, `mcp stop`, and `mcp status` accept `--json` for versioned machine output.

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

Queries run in read-only transactions. The query guard permits one `SELECT`-family statement and checks directly referenced relations against saved scope. By default, a profile allows a 60-second query budget, 500 returned rows, and a 1 MiB result limit; the query timeout can be configured up to five minutes. Data Mate stores nonsecret profiles and separate Tink-encrypted credential bundles in `.dev/data-mate/data-mate.db`. Production uses the same database layout under its fixed root. One serialized AES-256-GCM Tink keyset per installation lives in the macOS Keychain. The service retains the loaded keyset until stopped; ordinary connection additions and edits then require no further Keychain access. Initialization and unlocking after restart may prompt, especially after an unsigned development rebuild. Noninteractive callers receive an unlock-required error when OS interaction is needed. Scope-only changes and deletion need no unlock. No MCP tool accepts credentials or changes saved connections.

Managed data files live directly in `.dev/data-mate/`: the database, installation identity, optional `known_hosts`, locks, and service/registration records. The executable remains separate at `.dev/bin/data-mate`. Direct executable invocation resolves the same data root regardless of the working directory. Production uses the fixed OS-account-home path `~/.local/share/data-mate/` and records its executable, shell artifacts, and recovery state there.

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
| `make release`                           | Build signed/notarized production artifacts using explicit signing settings; never publish |
| `make uninstall PURGE=1`                   | Also delete owned profiles, credentials, host pins, and the installation keyset  |

To run only one PostgreSQL integration version, add `DB_IMAGE=postgres:16` or `DB_IMAGE=postgres:18`. Docker tests require a running Docker daemon and are excluded from CI. Native Keychain and launchd checks require an unlocked macOS user session and explicit opt-in:

```bash
DATA_MATE_NATIVE_TEST=1 go test -mod=readonly -run '^TestNativeKeychainLifecycle$' ./internal/vault
DATA_MATE_NATIVE_TEST=1 go test -mod=readonly -run '^TestNativeServiceLifecycle$' ./internal/service
```

Production release verification has a separate opt-in. Supply an already signed/notarized release candidate and its adjacent `release.txt`; the test verifies trust before execution and rejects modified executable code:

```bash
DATA_MATE_NATIVE_TEST=1 DATA_MATE_DISTRIBUTION_CANDIDATE=/absolute/path/data-mate_darwin_arm64 go test -mod=readonly -run '^TestNativeRelease$' ./internal/distribution
```

`make release` requires `DATA_MATE_SIGNING_IDENTITY` and `DATA_MATE_NOTARY_PROFILE`, a clean `v<VERSION>` tag, native arm64, and the pinned build tools. Release artifacts go to the ignored `.dist/` directory at the checkout root; move or remove existing output before rebuilding. `DATA_MATE_RELEASE_OUTPUT` optionally selects a different new absolute directory. The separate release workflow produces candidate artifacts; publication requires clean-host acceptance and verified immutable-release settings. The normal CI remains independent of signing credentials.

Source layout: `cmd/data-mate` is the executable entry point; `internal/cli` owns user commands; `internal/config` and `internal/vault` own profiles and credentials; `internal/database/postgres` and `internal/transport` own database access; `internal/service`, `internal/mcp`, and `internal/agent` own the service and agent integration; `internal/distribution` owns production acquisition, shell artifacts, and recoverable publication. See [AGENTS.md](AGENTS.md) for contribution and validation rules, [PRD](specs/PRD.md) for product scope, and [DESIGN](specs/DESIGN.md) for technical contracts.

## Development state compatibility

Before adopting the shared launchd label, stop older per-root services using their original binary. Verified version-2 SQLite installation identities retain their UUID, key account, profiles, and ciphertext when upgraded; obsolete JSON layouts below remain unsupported. Development `upgrade`, `update`, and `uninstall` exit 2 with Make guidance. Development root overrides and `make dev` remain supported, and development installation never edits shell startup files.

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
