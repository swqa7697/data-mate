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
| Connection scope      | Allow schemas through a whitelist or blacklist.                        |
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

`db add` opens a form for the connection details and a hidden password. Replace `analytics` with the alias you choose. `db test` checks connectivity and read-only transaction access. Open a new Codex or Claude Code session after `mcp start` so it can load the registration. You can then ask your agent to explore the database through Data Mate.

## Manage connections

| Command                         | Purpose                                             |
| ------------------------------- | --------------------------------------------------- |
| `data-mate db add`              | Save a PostgreSQL connection.                       |
| `data-mate db list`             | View aliases, nonsecret settings, and scopes.       |
| `data-mate db describe [alias]` | List accessible schemas/tables and allowed scope.   |
| `data-mate db test [alias]`     | Test one connection, or all connections if omitted. |
| `data-mate db edit [alias]`     | Update settings or credentials.                     |
| `data-mate db scope [alias]`    | Choose allowed schemas.                             |
| `data-mate db remove [alias]`   | Remove a connection and its saved credentials.      |
| `data-mate mcp status`          | Check service and agent registration status.        |
| `data-mate mcp stop`            | Stop agent access.                                  |

Inspect one saved database with `data-mate db describe analytics`, or omit the alias to choose a profile interactively. Use `--json` for versioned machine-readable output; scripts must supply an alias. The description groups readable tables, views, materialized views, partitioned tables, and foreign tables by schema, including empty accessible schemas. Each schema is labeled allowed or excluded by the saved scope, whose summary also states the future-schema policy. PostgreSQL privileges still determine which objects appear; “allowed” does not grant permissions.

Description connects through the management service without enabling MCP and can require a credential unlock. It reads catalog names only, with no columns or application rows. Results are complete within 4,096 combined schema/relation entries and the profile's result-byte and timeout limits; exceeding a limit fails without printing a partial description. MCP agents continue to see only the allowed catalog.

A new connection uses an empty blacklist, sharing all accessible application schemas, including future schemas, by default. Narrow its scope before starting MCP if needed:

```bash
data-mate db scope analytics
# Or select exact names without the interactive picker:
data-mate db scope analytics --schema reporting --schema public --yes
# Or allow everything except these schemas, including newly created schemas:
data-mate db scope analytics --exclude-schema private --exclude-schema staging --yes
```

`--schema` replaces the scope with a whitelist; `--exclude-schema` replaces it with a blacklist. Both accept repeated exact, case-sensitive schema names and include all current and future tables within allowed schemas. `--all` saves an empty blacklist; `--none` saves an empty whitelist. These options and `--scope-json` are mutually exclusive. JSON uses `{"mode":"blacklist","schemas":["private"]}` or `{"mode":"whitelist","schemas":["reporting"]}`.

In the colored picker, use arrows to move, Space to toggle a schema, `a` to toggle all schemas, and `m` to switch modes while preserving checkboxes. `/` searches, `n` advances pages, and `b` returns to the first page. Enter reviews changes before confirmation; `q` or Ctrl-C cancels. `NO_COLOR` disables color. Bulk actions cover all schemas regardless of search and preserve the mode: clearing all in blacklist mode still permits future schemas, whereas whitelist mode blocks them. The displayed mode always states the future-schema policy. Bulk actions and mode switching are limited to 4,096 names and a 30-second catalog scan; failure preserves the current selection.

Scope limits direct table references and catalog results; PostgreSQL privileges still apply. Run `data-mate db add --help` for TLS, SSH, proxy, query limits, and script input options. Passwords go through the form or standard input, never command-line values.

## What agents can do

| MCP tool           | Capability                                                                            |
| ------------------ | ------------------------------------------------------------------------------------- |
| `list_connections` | Find available aliases and scopes.                                                    |
| `list_tables`      | Browse visible tables.                                                                |
| `describe_table`   | Inspect columns, defaults, keys, indexes, triggers, views, and row-security policies. |
| `list_objects`     | Browse routines, explicit types, and sequences.                                       |
| `describe_object`  | Read routine source, enum labels, type details, and sequence configuration.           |
| `query`            | Run one read statement with optional parameters.                                      |

Queries run in read-only transactions. The default budget per query is **60 seconds, 500 rows, and 1 MiB of results**; connection settings can adjust these limits. MCP tools cannot edit connections or retrieve saved passwords.

## Update or remove

```bash
data-mate upgrade             # Install the latest stable release
data-mate mcp start           # Resume agent access after upgrading
data-mate uninstall           # Keep saved connections and credentials
data-mate uninstall --purge   # Remove saved connections, credentials, and their keyset
```

Uninstall shows what it will remove and asks for confirmation. Run `data-mate help` for the complete command reference.

## License

[GNU General Public License v3](LICENSE).
