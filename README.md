<h1 align="center">Data Mate</h1>

<p align="center">
  <strong>Explore PostgreSQL from Codex or Claude Code.</strong><br />
  A local CLI and <a href="https://modelcontextprotocol.io/">Model Context Protocol</a> (MCP) service for read-only database access on macOS and Linux.
</p>

<p align="center">
  <a href="https://github.com/swqa7697/data-mate/releases/latest"><img src="https://img.shields.io/github/v/release/swqa7697/data-mate" alt="Latest release" /></a>
  <img src="https://img.shields.io/badge/macOS-15%2B%20%C2%B7%20Apple%20Silicon-000000?logo=apple&logoColor=white" alt="macOS 15+ on Apple Silicon" />
  <img src="https://img.shields.io/badge/Linux-Ubuntu%2024.04%2B%20%C2%B7%20x86__64-FCC624?logo=linux&logoColor=black" alt="Linux x86_64 on Ubuntu 24.04+" />
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

| Feature               | What it gives you                                                             |
| --------------------- | ----------------------------------------------------------------------------- |
| Read-only tools       | List connections and tables, describe tables, and run bounded queries.        |
| Protected credentials | Keep saved credentials protected with macOS Keychain or Linux Secret Service. |
| Connection options    | Use direct TCP, verified TLS, an SSH jump host, or a SOCKS5 proxy.            |
| Agent setup           | Register the service with installed Codex and Claude Code clients.            |
| Shell completion      | Complete commands and saved aliases in Bash or zsh.                           |

## Get started

Data Mate supports **macOS 15+ on Apple Silicon**, **Linux x86_64 with glibc 2.39+** (Ubuntu 24.04+), and **PostgreSQL 16+**. Install [Codex](https://openai.com/codex/) or [Claude Code](https://claude.com/product/claude-code) to use its MCP tools. Use a dedicated, operator-managed read-only PostgreSQL account with access only to the data you intend to share.

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

`db add` opens a form for the connection details and a hidden password. Replace `analytics` with the alias you choose. `db test` checks connectivity and read-only account privileges. Open a new Codex or Claude Code session after `mcp start` so it can load the registration. You can then ask your agent to explore the database through Data Mate.

## Manage connections

| Command                         | Purpose                                                                   |
| ------------------------------- | ------------------------------------------------------------------------- |
| `data-mate db add`              | Save a PostgreSQL connection.                                             |
| `data-mate db list`             | View aliases and nonsecret settings.                                      |
| `data-mate db describe [alias]` | List application enums, tables, views, sequences, indexes, and functions. |
| `data-mate db test [alias]`     | Test one connection, or all connections if omitted.                       |
| `data-mate db edit [alias]`     | Update settings or credentials.                                           |
| `data-mate db remove [alias]`   | Remove a connection and its saved credentials.                            |
| `data-mate mcp status`          | Check service and agent registration status.                              |
| `data-mate mcp stop`            | Stop agent access.                                                        |

Inspect one saved database with `data-mate db describe analytics`, or omit the alias to choose a profile interactively.

Run `data-mate db add --help` for connection options. Passwords go through the form or standard input, never command-line values.

## What agents can do

| MCP tool           | Capability                                                                            |
| ------------------ | ------------------------------------------------------------------------------------- |
| `list_connections` | Find available aliases and database labels.                                           |
| `list_tables`      | Browse visible tables.                                                                |
| `describe_table`   | Inspect columns, defaults, keys, indexes, triggers, views, and row-security policies. |
| `list_objects`     | Browse routines, types, and sequences.                                                |
| `describe_object`  | Read routine source, enum labels, type details, and sequence configuration.           |
| `query`            | Run one read statement with optional parameters.                                      |

MCP tools cannot edit connections or retrieve saved passwords.

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
