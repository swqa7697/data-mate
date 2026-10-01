<h1 align="center">Data Mate</h1>

<p align="center">
  <strong>Explore PostgreSQL, MySQL and MariaDB from Codex or Claude Code.</strong><br />
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
| Databases             | Connect to PostgreSQL 16+, MySQL 8.4+, and MariaDB 10.11+.                    |
| Protected credentials | Keep saved credentials protected with macOS Keychain or Linux Secret Service. |
| Connection options    | Use direct TCP, verified TLS, an SSH jump host, or a SOCKS5 proxy.            |
| Agent setup           | Register the service with installed Codex and Claude Code clients.            |
| Shell completion      | Complete commands and saved aliases in Bash or zsh.                           |

## Get started

Data Mate supports **macOS 15+ on Apple Silicon**, **Linux x86_64 with glibc 2.39+** (Ubuntu 24.04+), and **PostgreSQL 16+, MySQL 8.4+, or MariaDB 10.11+**. Install [Codex](https://openai.com/codex/) or [Claude Code](https://claude.com/product/claude-code) to use its MCP tools. Use a dedicated, operator-managed read-only database account with access only to the data you intend to share; see [Read-only accounts](#read-only-accounts).

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

`db add` opens a form for the driver (`postgres`, `mysql`, or `mariadb`), connection details, and a hidden password. The port defaults to the driver's standard port: 5432 for PostgreSQL and 3306 for MySQL and MariaDB. Replace `analytics` with the alias you choose. `db test` checks connectivity and read-only account privileges. Open a new Codex or Claude Code session after `mcp start` so it can load the registration. You can then ask your agent to explore the database through Data Mate.

## Manage connections

| Command                          | Purpose                                                                   |
| -------------------------------- | ------------------------------------------------------------------------- |
| `data-mate db add`               | Save a PostgreSQL, MySQL, or MariaDB connection.                          |
| `data-mate db list`              | View aliases and nonsecret settings.                                      |
| `data-mate db describe [alias]`  | List application enums, tables, views, sequences, indexes, and functions. |
| `data-mate db test [alias]`      | Test one connection, or all connections if omitted.                       |
| `data-mate db edit [alias]`      | Update settings or credentials.                                           |
| `data-mate db remove [alias]`    | Remove a connection and its saved credentials.                            |
| `data-mate db export <file.csv>` | Export connections to a CSV file.                                         |
| `data-mate db import <file.csv>` | Import connections from a CSV file.                                       |
| `data-mate mcp status`           | Check service and agent registration status.                              |
| `data-mate mcp stop`             | Stop agent access.                                                        |

Inspect one saved database with `data-mate db describe analytics`, or omit the alias to choose a profile interactively.

Run `data-mate db add --help` for connection options. Passwords go through the form or standard input, never command-line values.

To copy connections to another machine, run `data-mate db export connections.csv`, then `data-mate db import connections.csv` there. Import asks for any passwords the file leaves empty.

A PostgreSQL connection names one database. A MySQL or MariaDB connection names only a server account: it has no database setting, and agents reach every database the account's grants allow. In tool results, a MySQL or MariaDB database appears as a schema, and queries name tables as `database.table`.

## Read-only accounts

Data Mate checks the account's privileges when you save a connection, on every `db test`, and before a new connection pool is used. It rejects accounts that can write data, change definitions, manage accounts or roles, or write files. The server's own privileges decide what agents can see and query, so grant only what you intend to share.

PostgreSQL:

```sql
CREATE ROLE analytics_reader LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE analytics TO analytics_reader;
GRANT USAGE ON SCHEMA app TO analytics_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA app TO analytics_reader;
```

MySQL or MariaDB, where grants choose the reachable databases:

```sql
CREATE USER 'analytics_reader'@'%' IDENTIFIED BY '...';
GRANT SELECT, SHOW VIEW ON analytics.* TO 'analytics_reader'@'%';
```

For MySQL and MariaDB, read-only privileges include `SELECT`, `SHOW VIEW`, `SHOW DATABASES`, `EXECUTE`, `PROCESS`, `LOCK TABLES`, replication monitoring, and `SHOW_ROUTINE` (MySQL) or `SHOW CREATE ROUTINE` (MariaDB) to read routine source. Any other privilege is rejected, including `CREATE TEMPORARY TABLES`, as are roles that hold one. Never grant `SELECT` on the `mysql` system database, which exposes password hashes. Use TLS or SSH for connections beyond the local machine.

## What agents can do

| MCP tool           | Capability                                                                            |
| ------------------ | ------------------------------------------------------------------------------------- |
| `list_connections` | Find available aliases and database labels.                                           |
| `list_tables`      | Browse visible tables.                                                                |
| `describe_table`   | Inspect columns, defaults, keys, indexes, triggers, views, and row-security policies. |
| `list_objects`     | Browse routines, types, and sequences.                                                |
| `describe_object`  | Read routine source, enum labels, type details, and sequence configuration.           |
| `query`            | Run one read statement with optional parameters.                                      |

Results follow each connection's database, reported by `list_connections`:

- PostgreSQL lists routines, types, and sequences, and reports identity columns, triggers, and row-security policies. Queries use `$1` placeholders and schema-qualified table names.
- MySQL and MariaDB list functions and procedures, and MariaDB also lists sequences. Table descriptions report `AUTO_INCREMENT`, `ON UPDATE`, and the storage engine. MySQL shows triggers only to accounts with the `TRIGGER` privilege, which read-only accounts lack, so they are omitted. Queries use `?` placeholders and `database.table` names.

MCP tools cannot edit connections or retrieve saved passwords.

## Update or remove

```bash
data-mate upgrade             # Install the latest stable release
data-mate mcp start           # Resume agent access after upgrading
data-mate uninstall           # Keep saved connections and credentials
data-mate uninstall --purge   # Remove saved connections, credentials, and their keyset
```

Uninstall shows what it will remove and asks for confirmation. Saved connections and credentials remain available after reinstalling unless you use `--purge`. Add `--yes` to skip confirmation; purging protected credentials can still require keyring authentication.

Run `data-mate help` for the complete command reference.

## License

[GNU General Public License v3](LICENSE).
