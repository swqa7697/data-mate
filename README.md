<h1 align="center">Data Mate</h1>

<p align="center">
  <strong>Explore PostgreSQL from Codex or Claude Code.</strong><br />
  A local CLI and <a href="https://modelcontextprotocol.io/">Model Context Protocol</a> (MCP) service for read-only database access on macOS.
</p>

<p align="center">
  <a href="https://github.com/swqa7697/data-mate/releases/latest"><img src="https://img.shields.io/github/v/release/swqa7697/data-mate" alt="Latest release" /></a>
  <img src="https://img.shields.io/badge/macOS-15%2B%20%C2%B7%20Apple%20Silicon-000000?logo=apple&logoColor=white" alt="macOS 15+ on Apple Silicon" />
  <img src="https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white" alt="Go 1.27" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-GPL--3.0-blue" alt="GPL-3.0 license" /></a>
</p>

<p align="center">
  <a href="#get-started">Get started</a> ·
  <a href="#manage-connections">Manage connections</a> ·
  <a href="#what-agents-can-do">Agent tools</a> ·
  <a href="#update-or-remove">Update or remove</a>
</p>

---

Data Mate lets coding agents discover tables, inspect columns, and answer questions with SQL. You control each connection and the schemas agents can see. Database passwords stay out of agent configuration and MCP responses.

## At a glance

| Feature               | What it gives you                                                      |
| --------------------- | ---------------------------------------------------------------------- |
| Read-only tools       | List connections and tables, describe tables, and run bounded queries. |
| Protected credentials | Encrypt saved credentials with a keyset held in macOS Keychain.        |
| Connection options    | Use direct TCP, verified TLS, an SSH jump host, or a SOCKS5 proxy.     |
| Agent setup           | Register the service with installed Codex and Claude Code clients.     |
| Shell completion      | Complete commands and saved aliases in Bash or zsh.                    |

## Get started

Data Mate supports **macOS 15 or later on Apple Silicon** and **PostgreSQL 16 or later**. Install [Codex](https://openai.com/codex/) or [Claude Code](https://claude.com/product/claude-code) to use its MCP tools. Use a dedicated, operator-managed read-only PostgreSQL account with access only to the data you intend to share.

Install the latest stable release:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://github.com/swqa7697/data-mate/releases/latest/download/install.sh | /bin/bash
```

Open a new terminal after installation so `data-mate` is on your path. Then add a connection, test it, and enable agent access:

```bash
data-mate db add
data-mate db test analytics
data-mate mcp start
```

`db add` opens a form for the connection details and a hidden password. Replace `analytics` with the alias you choose. `db test` checks connectivity, read-only account privileges, and transaction enforcement using a fresh connection. Open a new Codex or Claude Code session after `mcp start` so it can load the registration. You can then ask your agent to explore the database through Data Mate.

## Manage connections

| Command                         | Purpose                                             |
| ------------------------------- | --------------------------------------------------- |
| `data-mate db add`              | Save a PostgreSQL connection.                       |
| `data-mate db list`             | View aliases and nonsecret settings.                |
| `data-mate db describe [alias]` | List database schemas and table metadata.        |
| `data-mate db test [alias]`     | Test one connection, or all connections if omitted. |
| `data-mate db edit [alias]`     | Update settings or credentials.                     |
| `data-mate db remove [alias]`   | Remove a connection and its saved credentials.      |
| `data-mate mcp status`          | Check service and agent registration status.        |
| `data-mate mcp stop`            | Stop agent access.                                  |

Inspect one saved database with `data-mate db describe analytics`, or omit the alias to choose a profile interactively. Use `--json` for versioned machine-readable output; scripts must supply an alias. The description groups tables, views, materialized views, partitioned tables, and foreign tables by schema, including system and empty schemas. Metadata follows PostgreSQL catalog permissions; seeing a definition does not grant access to its rows.

Description connects through the management service without enabling MCP and can require a credential unlock. It reads catalog names only, with no columns or application rows. Results are complete within 4,096 combined schema/relation entries and the profile's result-byte and timeout limits; exceeding a limit fails without printing a partial description.

Every confirmed `db add` or `db edit`, including an unchanged edit, must connect and validate a read-only account before saving. Unreachable databases, writable accounts, and invalid credentials prevent the save and preserve the previous profile and credentials. Edits may require unlocking existing credentials; removal and passive listing remain available without database access.

Use a dedicated non-owner account with CONNECT, schema USAGE, and SELECT on the intended data. Data Mate rejects persistent write/create privileges, sequence USAGE/UPDATE, ownership, administrative capabilities, and write privileges inherited through roles or PUBLIC. TEMP privileges and PostgreSQL's default session-setting access remain allowed. Data Mate does not change grants for you.

Saved connections are validated when a pool opens, before any operation uses it. Approval is reused while at least one physical connection remains and discarded when the last disconnect is observed, the profile changes, or the pool retires. There is no per-query privilege audit or persistent approval stamp. `db test` always revalidates independently. Privilege changes during an active pool are detected at the next fresh audit; every operation still uses a read-only transaction.

Queries require schema-qualified relation names and can read system schemas. Application and extension routines, `SHOW`, and `EXPLAIN`/`EXPLAIN ANALYZE` of read queries are available. PostgreSQL enforces data grants and read-only transactions. The server, administrator, installed code, and external capabilities are trusted: this prevents ordinary persistent database writes, but is not a sandbox for arbitrary server-side code or separate external connections.

Run `data-mate db add --help` for TLS, SSH, proxy, query limits, and script input options. Passwords go through the form or standard input, never command-line values.

## What agents can do

| MCP tool           | Capability                                                                            |
| ------------------ | ------------------------------------------------------------------------------------- |
| `list_connections` | Find available aliases and database labels.                                           |
| `list_tables`      | Browse visible tables.                                                                |
| `describe_table`   | Inspect columns, defaults, keys, indexes, triggers, views, and row-security policies. |
| `list_objects`     | Browse routines, types, and sequences.                                       |
| `describe_object`  | Read routine source, enum labels, type details, and sequence configuration.           |
| `query`            | Run one read statement with optional parameters.                                      |

Queries run in read-only transactions. The default budget per query is **60 seconds, 500 rows, and 1 MiB of results**; connection settings can adjust these limits. MCP tools cannot edit connections or retrieve saved passwords.

## Update or remove

**Breaking change in Unreleased:** saved profiles containing `scope` are rejected without modification. Before upgrading from a release or development build with scope settings, use its executable to run `data-mate db list --json`, record the nonsecret connection settings, and remove each connection with `data-mate db remove <alias>`. After upgrading, recreate connections with `db add` and re-enter their credentials. There is no automatic migration; the SQLite layout and JSON version numbers remain unchanged. Restrict PostgreSQL grants before enabling MCP.

```bash
data-mate upgrade             # Install the latest stable release
data-mate mcp start           # Resume agent access after upgrading
data-mate uninstall           # Keep saved connections and credentials
data-mate uninstall --purge   # Remove saved connections, credentials, and their keyset
```

Uninstall shows what it will remove and asks for confirmation. Run `data-mate help` for the complete command reference.

## License

[GNU General Public License v3](LICENSE).
