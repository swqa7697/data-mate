<h1 align="center">Data Mate</h1>

<p align="center">
  <strong>Explore PostgreSQL, MySQL, MariaDB, Redis and MongoDB from Codex or Claude Code.</strong><br />
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
  <a href="#supported-databases">Supported databases</a> ·
  <a href="#get-started">Get started</a> ·
  <a href="#manage-connections">Manage connections</a> ·
  <a href="#what-agents-can-do">Agent tools</a> ·
  <a href="#update-or-remove">Update or remove</a>
</p>

---

Data Mate lets coding agents inspect relational databases with SQL, explore Redis by key, and read MongoDB collections. You control each connection and the data its account can read. Database passwords stay out of agent configuration and MCP responses.

## At a glance

| Feature               | What it gives you                                                             |
| --------------------- | ----------------------------------------------------------------------------- |
| Read-only tools       | Inspect relational catalogs, run SQL reads, browse Redis keys and values, and query MongoDB collections. |
| Protected credentials | Keep saved credentials protected with macOS Keychain or Linux Secret Service. |
| Connection options    | Use direct TCP, verified TLS, an SSH jump host, or a SOCKS5 proxy.            |
| Agent setup           | Register the service with installed Codex and Claude Code clients.            |
| Shell completion      | Complete commands and saved aliases in Bash or zsh.                           |

## Supported databases

| Database | Minimum version | Driver in `db add` |
| --- | --- | --- |
| PostgreSQL | 16 | `postgres` |
| MySQL | 8.4 | `mysql` |
| MariaDB | 10.11 | `mariadb` |
| Redis (standalone) | 7.0 | `redis` |
| [MongoDB](#mongodb-reader) | 7.0 | `mongodb` |

## Get started

Data Mate runs on **macOS 15+ on Apple Silicon** and **Linux x86_64 with glibc 2.39+** (Ubuntu 24.04+). Install [Codex](https://openai.com/codex/) or [Claude Code](https://claude.com/product/claude-code) to use its MCP tools. Choose an account with access only to the data you intend to share. See [Read-only accounts](#read-only-accounts) for each driver’s requirements.

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

`db add` opens a form for the driver (`postgres`, `mysql`, `mariadb`, `redis`, or `mongodb`), connection details, and a hidden password. The port defaults to the driver's standard port: 5432 for PostgreSQL, 3306 for MySQL and MariaDB, 6379 for Redis, or 27017 for MongoDB. Replace `analytics` with the alias you choose. `db test` checks connectivity and authentication. For Redis it verifies the server version, standalone mode and selected database. For SQL and MongoDB it also checks read-only account privileges. Open a new Codex or Claude Code session after `mcp start` so it can load the registration. You can then ask your agent to explore the database through Data Mate.

## Manage connections

| Command                          | Purpose                                                                   |
| -------------------------------- | ------------------------------------------------------------------------- |
| `data-mate db add`               | Save a database connection.                          |
| `data-mate db list`              | View aliases and nonsecret settings.                                      |
| `data-mate db describe [alias]`  | Describe a relational or MongoDB catalog, or summarize Redis key patterns and types. |
| `data-mate db test [alias]`      | Test one connection, or all connections if omitted.                       |
| `data-mate db edit [alias]`      | Update settings or credentials.                                           |
| `data-mate db remove [alias]`    | Remove a connection and its saved credentials.                            |
| `data-mate db export <file.csv>` | Export connections to a CSV file.                                         |
| `data-mate db import <file.csv>` | Import connections from a CSV file.                                       |
| `data-mate mcp status`           | Check service and agent registration status.                              |
| `data-mate mcp stop`             | Stop agent access.                                                        |

Inspect one saved database with `data-mate db describe analytics`, or omit the alias to choose a profile interactively. In a terminal, the alias shows a loading indicator while the command fetches the description.

For Redis, `db describe` checks up to eight candidate keys in the selected database. It groups readable keys by type and by repeated prefixes such as `orders:*`. When keys have no shared prefix, the description shows up to three example key names per type. Long names show the first 96 bytes or fewer and carry a truncation marker. Counts apply only to this sample. An empty sample can occur even in a populated database. Use the MCP tool `list_keys` with a reported pattern to browse matching keys. For scripts, Redis description JSON is now version 2. The `keyspace` object contains `observed_keys` and `groups`. Each group has a `type` and a `count`. Groups with a `pattern` can be explored with `list_keys`. Other groups include `examples`. Pass the `encoding` and `value` of a complete example to `describe_key`. An example with `truncated: true` is a preview of a longer name.

Run `data-mate db add --help` for connection options. Passwords go through the form or standard input, never command-line values.

To copy connections to another machine, run `data-mate db export connections.csv`, then `data-mate db import connections.csv` there. Import asks for any passwords the file leaves empty.

A PostgreSQL connection names one database. A MySQL or MariaDB connection names only a server account: it has no database setting, and agents reach every database the account's grants allow. In tool results, a MySQL or MariaDB database appears as a schema, and queries name tables as `database.table`.

For Redis, enter the ACL username and a logical database index (default `0`). You can also set the index with `--database` or the CSV `database` column. Each alias connects to one standalone endpoint.

For MongoDB, enter the username and the database where the user was created, usually `admin` (the default). The account's roles choose which databases agents can read. Each alias connects directly to one standalone server, replica-set member or `mongos`.

## Read-only accounts

PostgreSQL, MySQL, MariaDB and MongoDB require a dedicated read-only account. For these drivers, Data Mate checks the account's privileges when you save a connection, on every `db test`, and before a new connection pool is used. After 30 minutes, the next pooled operation must pass a fresh check. Data Mate rejects accounts that can write data, change definitions, manage accounts or roles, or write files. The server's own privileges decide what agents can see and query, so grant only what you intend to share.

For PostgreSQL, the audit permits effective membership in `pg_read_all_data`, `pg_monitor`, `pg_read_all_settings`, `pg_read_all_stats`, `pg_stat_scan_tables`, and `pg_use_reserved_connections`. Other predefined roles, including `pg_signal_backend`, fail the audit. A typical reader needs only ordinary grants.

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

### Redis reader

You can use an existing Redis account, including one with write privileges or one on a managed service that blocks ACL inspection. Data Mate’s Redis driver sends only commands for connection setup, key discovery, metadata and value reads. Redis enforces the account’s permissions on each command. Data Mate checks that the account can read a key’s value before showing its name or metadata.

If you manage Redis ACLs, this optional ACL file entry enables all three key tools for keys matching `orders:*`. Replace `<sha256-of-password>` with the 64 hexadecimal digits of the reader’s password hash. Enter the original password in the Data Mate form.

```text
user data_mate_reader on #<sha256-of-password> %R~orders:* -@all +hello +select +scan +type +pttl +strlen +hlen +llen +scard +zcard +xlen +getrange +hscan +lrange +sscan +zrange +xrange
```

### MongoDB reader

Create a dedicated user in `mongosh` as an administrator. This example lets the reader query the `analytics` database:

```js
db.getSiblingDB("admin").createUser({
  user: "data_mate_reader",
  pwd: passwordPrompt(),
  roles: [{ role: "read", db: "analytics" }]
})
```

Keep `admin`, where this user was created, as the authentication database. Grant `read` on each database you want to share, or `readAnyDatabase` for all of them. Data Mate rejects accounts that can do more than read and monitor, accounts that can read credential collections such as `admin.system.users`, and servers that do not enforce authorization. On MongoDB 8.2 and later it also accepts `clusterMonitor`, but on a replica set that role can read the oplog, which records every write, including credential changes; grant it only when that exposure is acceptable.

Data Mate checks privileges and authorization enforcement on every new pooled MongoDB connection. The check requires `usersInfo`, a command unavailable on MongoDB Atlas. [MongoDB command compatibility](https://www.mongodb.com/docs/manual/reference/command/usersinfo/)

## What agents can do

| MCP tool           | Capability                                                                            |
| ------------------ | ------------------------------------------------------------------------------------- |
| `list_connections` | Find available aliases, database labels, and current finite execution limits.         |
| `list_tables`      | Browse visible tables.                                                                |
| `describe_table`   | Inspect columns, defaults, keys, indexes, triggers, views, and row-security policies. |
| `list_objects`     | Browse routines, types, and sequences.                                                |
| `describe_object`  | Read routine source, enum labels, type details, and sequence configuration.           |
| `query`            | Run one read statement with optional parameters.                                      |
| `list_keys`        | Browse readable Redis keys and types, optionally matching a Redis glob.              |
| `describe_key`     | Inspect a Redis key’s type, length and remaining lifetime.                          |
| `read_key`         | Read a page of string bytes, list or set members, hash fields, scores or stream entries. |
| `list_collections` | Browse MongoDB collections, views, and time-series collections.                       |
| `describe_collection` | Inspect a MongoDB collection's options, validator, view pipeline, and indexes. |
| `find_documents`   | Run one MongoDB find with an optional filter, projection, and sort.                   |
| `aggregate_documents` | Run one read-only MongoDB aggregation pipeline. |

Results follow each connection's database, reported by `list_connections`:

- PostgreSQL lists routines, types, and sequences, and reports identity columns, triggers, and row-security policies. Queries use `$1` placeholders and schema-qualified table names.
- MySQL and MariaDB list functions and procedures, and MariaDB also lists sequences. Table descriptions report `AUTO_INCREMENT`, `ON UPDATE`, and the storage engine. MySQL shows triggers only to accounts with the `TRIGGER` privilege, which read-only accounts lack, so they are omitted. Queries use `?` placeholders and `database.table` names.
- Redis readers can copy a `key` object from `list_keys` into either key tool, for example `{"encoding":"utf8","value":"orders:42"}`. Binary keys and values use base64. The default page size is 100, capped by the connection’s row limit. An explicit page can request up to 1,000 bytes for strings or 1,000 entries for collections, subject to that same limit. Follow `next_cursor` even after an empty page. Iteration over changing data may repeat entries or require a restart.
- MongoDB tools name a `database` and `collection`. Filters, projections, sorts and pipelines use MongoDB Extended JSON, and documents come back as canonical Extended JSON, such as `{"$numberLong":"42"}`. Results stop at the row limit without a continuation; refine the filter, use `skip` with a `sort`, or count with a `$count` stage. Pipelines cannot use `$out`, `$merge` or server-side JavaScript.

MCP tools cannot edit connections or retrieve saved passwords.

Tool failures, invalid requests and concurrent request saturation preserve the MCP session. Correct the request or its cause and retry in the same agent session. Actual transport failures and service shutdown still end the connection.

## Update or remove

```bash
data-mate upgrade             # Install the latest stable release
data-mate mcp start           # Resume agent access after upgrading
data-mate uninstall           # Keep saved connections and credentials
data-mate uninstall --purge   # Remove saved connections, credentials, and their keyset
```

Uninstall shows what it will remove and asks for confirmation. Saved connections and credentials remain available after reinstalling unless you use `--purge`. Add `--yes` to skip confirmation; purging protected credentials can still require keyring authentication.

Run `data-mate help` for the complete command reference.

## Upgrade development dependencies

From the source checkout, run `make upgrade` to update direct Go dependencies and developer tools noninteractively. The command uses the version of `go-mod-upgrade` pinned in `go.mod` and finishes with `go mod tidy`.

Use `make upgrade MAJOR=1` to discover new major module paths and rewrite imports. Crossing several major versions can take multiple runs. Transitive dependencies can change in either mode. Review the diff, then run `make lint` and `make test`.

## CI caches

Pull requests targeting `main` or `dev` restore separate Go module and build caches on macOS and Linux. Creating a stable release tag such as `v1.2.3` triggers `release-cache-signal`. After that signal succeeds, `go-cache-warm` checks out the `main` revision, runs setup, lint and build, and compiles both ordinary and race tests with `make compile-tests`. The warmed caches belong to `main`, so subsequent pull requests can reuse them. Both cache workflows must be on `main` before creating the tag.

## License

[GNU General Public License v3](LICENSE).
