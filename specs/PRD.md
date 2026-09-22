# Data Mate - Creating A Local MCP Service For DB Connections Elegantly

## Concept

Data Mate is a lightweight CLI tool written in Go, aiming to elegantly setup a local MCP service running in background, using users provided DB connections, and make it to be found and used by terminal coding agents -- allowing agents to get contexts of a specified Database and to query data (read-only) for analysis.

## Requirements

- Mainstream Agent Supports: Now add supports for Codex and Claude Code
  - Abstracted Layers: One common logic layer + multiple agent layers
  - More Supports: Be able to add more agents to be compatible in the future
- Broad DB Supports: Supports all mainstream DB types, such as PG, MySQL, MongoDB, etc.
  - First Implementation: Add PG supports in the first version (min 16), and should be able to add more later without pain because of abstraction design
- OS Supports: Test and run only on macOS (Apple Silicon) for now, and may add Linux in the future (never supports Windows or Intel x86 macOS)
- Abstraction: Well-designed abstraction code structure, for better code quality and easier maintenance
  - Adapter Layer: agent supports are abstracted
  - Driver Layer: DB supports are abstracted
- Read Only: The MCP service provides only read-only tools and permissions to agents
- Flexible Scope: Users are able to configure the visible schemas or tables for a on file DB connection
- Simple Management: Users can conveniently and simply add/remove/modify a DB connection without pain
- Security: Keep nonsecret connection profiles in files, never leak credentials in plain text, and never expose credentials to agents.
- Full CLI integration: standard + interactive CLI user experience

## Implementation Decisions

- Development language: Go
- Agent Guidelines: Create project guidelines in root level `AGENTS.md` and an additional `CLAUDE.md` pointing to it
- Developer Tools: Wire up a root level `Makefile` and bash scripts under `scripts` for commonly used commands, including:
  - `install`/`build`: dev install, including installing dependencies, build binary, and install binary to `.dev`
  - `clean`: Clean build artifacts
  - `uninstall`: dev uninstall, removing all build artifacts and dev installation, and removing configs & creds (all things) only when PURGE=1 provided
  - `format`/`tidy`: Run formatter (Go source codes and bash scripts)
  - `lint`: Run lint check againt Go source codes
  - `test`: Run test suite (regression tests and unit tests)
  - `test-integration`: Run optional integration tests involving Docker containers
- Test Suite: Regression tests and optional unit tests for logic checks and CI, plus an optional real integration tests against Docker based local DB instances (not run in CI)
  - Configurable Test Image: Can test against any DB types or versions available as Docker images; for the first implementation, test PG 16 and 18
- Git ignored directories:
  - `.dev`: For binary and configs of development environment
  - `.misc`: Implementation plan, status tracking, test results, complex logs, and binary environments (if needed)
  - `.tmp`: Developer managed stuffs only -- agents shouldn't write in it

### Distribution

- Deferred: No distribution for now, only `.dev` installation
- Shortcut: Developers can use `make dev ARGS=""` for shortcut to run the built binary under `.dev` instead of using full path of it

### CLI Experience & DB Connections

Pretty and colored CLI experience. Scrathed commands design below.

- DB - Interactive | preivew & confirm (Y/N)
  - `db add`: Config "driver (PG only this version), connection alias ("c-alias" below), host, port, DB name, username, password" only -- all advanced options are disabled by default
  - `db add [add-on-options]`: Add optional flags to add connection with advanced options like SSL/TLS, SSH tunnel and proxy
  - `db scope [c-alias]`: Select visible schemas and tables from available list; Use "space" to select and "enter" to confirm -- similar experience with the `pnpm dlx taze -I` provides; A newly added connection has all schemas and tables exposed by default
  - `db edit [c-alias(optional)]`: Modify a DB connection
  - `db remove [c-alias(optional)]` / `db rm [c-alias(optional)]`: Remove a DB connection
- DB - One shot
  - `db list` / `db ls`: List all available DB connections with their visible scopes
  - `db test [c-alias(optional)]`: Test connections; test all connections by default, or provide a c-alias to check one connection
- MCP - One shot
  - `mcp start`: Start the MCP service to expose all available DB connections with configured visible scope, to all supported agents
  - `mcp stop`: Stop the MCP service
  - `mcp status`: Show status of the MCP service
- General - One Shot
  - `upgrade` / `update`: Update to latest distributed stable version; Stub until distribution lands
  - `help`, `version`, etc.

**No Start-Agent Commands**: All agents are decoupled from Data Mate -- any session can find and use an active Data Mate MCP service to get information requested by users. Don't require users to do anything other than running `data-mate mcp start` before starting an agent session to analysis data. Configuring the visible scopes is optional, based on users needs.
