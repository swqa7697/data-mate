# Data Mate - Creating A Local MCP Service For DB Connections Elegantly

## Concept

Data Mate is a lightweight CLI tool written in Go, aiming to elegantly setup a local MCP service running in background, using users provided DB connections, and make it to be found and used by terminal coding agents -- allowing agents to get contexts of a specified Database and to query data (read-only) for analysis.

## Requirements

- Mainstream Agent Supports: Now add supports for Codex and Claude Code
  - Abstracted Layers: One common logic layer + multiple agent layers
  - More Supports: Be able to add more agents to be compatible in the future
- Broad DB Supports: Supports all mainstream DB types, such as PG, MySQL, MongoDB, etc.
  - First Implementation: Add PG supports in the first version (min 16), and should be able to add more later without pain because of abstraction design
  - MySQL and MariaDB: Supported through the same driver abstraction (MySQL min 8.4, MariaDB min 10.11); a MySQL-family connection names a server account, and grants choose its reachable databases
- OS Supports: macOS 15+ (Apple Silicon) and Linux x86_64 (Windows, Intel macOS, Linux arm64 are unsupported)
- Abstraction: Well-designed abstraction code structure, for better code quality and easier maintenance
  - Adapter Layer: agent supports are abstracted
  - Driver Layer: DB supports are abstracted
- Read Only: The MCP service provides only read-only tools and permissions to agents, and also enforces read-only DB accounts
- Database Access: The database's own privileges determine data access, including newly created objects and system schemas
- Database Context: Agents can inspect catalog-visible routines and their source, enum and other type details (including arrays and table-row types), sequence configuration, and table/view definitions including defaults, indexes, constraints, triggers, and row-security policies. Inspect definitions without invoking routines or evaluating stored expressions
- Simple Management: Users can simply add/remove/modify a DB connection
- Security: Keep nonsecret connection profiles in files, never leak credentials in plain text, and never expose credentials to agents
- Full CLI integration: standard + interactive CLI user experience

## Implementation Decisions

- Development language: Go
- Agent Guidelines: Create project guidelines in root level `AGENTS.md` and an additional `CLAUDE.md` pointing to it
- Developer Tools: Wire up a root level `Makefile` and bash scripts under `scripts` for commonly used commands, including:
  - `install`/`build`: dev install, including installing dependencies, build binary, and install binary to `.dev`
  - `uninstall`: dev uninstall, removing all build artifacts and dev installation, and removing configs & creds (all things) only when PURGE=1 provided
  - `clean`: Alias of `uninstall` (without purge)
  - `format`/`tidy`: Run formatter (Go source codes and bash scripts)
  - `lint`: Run lint check againt Go source codes
  - `test`: Run test suite (regression tests and unit tests)
  - `test-integration`: Run optional integration tests involving Docker containers
- Test Suite: Regression tests and optional unit tests for logic checks and CI, plus an optional real integration tests against Docker based local DB instances (not run in CI)
  - Configurable Test Image: Can test against any DB types or versions available as Docker images; test PG 16 and 18, MySQL 8.4 and 9.7, and MariaDB 10.11 and 12.3
- Git ignored directories:
  - `.dev`: For binary and configs of development environment
  - `.misc`: Implementation plan, status tracking, test results, complex logs, and binary environments (if needed)
  - `.tmp`: Developer managed stuffs only -- agents shouldn't write in it

### Distribution

#### Production

- Terminal Installation: Simply installed to run a `curl` command in terminal to install latest stable standalone distribution
  - Location: Install for user (not root), and store configs (DB connection profiles) and other stuffs under the fixed `~/.local/share/data-mate` root. Production does not accept `--root` or another configuration-root override.
- Distribution Installer: A installation bash script (also in `scripts`) for users to install latest stable standalone binary through terminal
- Self-Contained: Not requiring user to have Go installed
- Standalone Upgrades: Run `data-mate upgrade` (or its alias `data-mate update`) to upgrade to the latest stable release, reusing the same upgrade path as the distribution installer
- Complete Terminal Uninstall: `data-mate uninstall` stops Data Mate and removes its executable, MCP registrations, runtime files, logs, caches, preferences, and other installation artifacts; retain only configured DB connections and their reusable encrypted credential store, required store metadata, and OS credential-store encryption keys.
  - Explicit Full Removal: `data-mate uninstall --purge` removes anything, including saved connections, encrypted credential store and associated OS credential-store keys, leaving literally **no residue**
  - Scope: No-residue cleanup covers Data Mate managed artifacts, not OS snapshots, user managed backups, external agent transcripts, or independent manual copies

#### Development

- Isolated: Development environment (binary, configs/profiles, etc.) is isolated from production installation
- Root Selection: Development retains `--root` and its checkout-local default; the fixed production root does not change development invocation or fixtures.
- Shortcut: Developers can use `make dev ARGS=""` for shortcut to run the built binary under `.dev` instead of using full path of it

### CLI Experience & DB Connections

Pretty and colored CLI experience. Scrathed commands design below.

- DB - Interactive | preivew & confirm (Y/N)
  - `db add`: Config "driver (postgres, mysql or mariadb), connection alias ("c-alias" below), host, port, DB name (PostgreSQL only), username, password" only -- all advanced options are disabled by default
  - `db add [add-on-options]`: Add optional flags to add connection with advanced options like SSL/TLS, SSH tunnel and proxy
  - `db edit [c-alias(optional)]`: Modify a DB connection
  - `db remove [c-alias(optional)]` / `db rm [c-alias(optional)]`: Remove a DB connection
  - `db import <csv-file>`: Add, skip or update multiple DB connections from a CSV file; secrets are optional in the file and missing ones are entered in the terminal
- DB - One shot
  - `db list` / `db ls`: List all available DB connections
  - `db describe [c-alias(optional)]`: Print enums, tables, views, sequences, indexes and functions under each schema of a DB connection
  - `db test [c-alias(optional)]`: Test connections; test all connections by default, or provide a c-alias to check one connection
  - `db export <csv-file>`: Export all DB connections to a CSV file without secrets; a secret a connection does not use is marked explicitly as `<none>`
- MCP - One shot
  - `mcp start`: Start the MCP service to expose all available DB connections to all supported agents
  - `mcp stop`: Stop the MCP service
  - `mcp status`: Print status of the MCP service
- General - One Shot
  - `upgrade` / `update`: Update to latest distributed stable version; not available for developement environment
  - `uninstall [--purge]`: Uninstall; not available for developement environment
  - `help`: Print help messages
  - `version`: Print version

**No Start-Agent Commands**: All agents are decoupled from Data Mate -- any session can find and use an active Data Mate MCP service to get information requested by users. Don't require users to do anything other than running `data-mate mcp start` before starting an agent session to analysis data.

### Singleton & ENV Exclusive

- Singleton: The MCP service (`data-mate mcp start`) is singleton per environment
- First Wins: When development and production envrionments are installed to the same machine, they can only start one service (with their own managed DB connections), but not two
