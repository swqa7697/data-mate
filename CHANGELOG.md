# Changelog

## [Unreleased]

### Added

- Add a checkout-local macOS Apple Silicon CLI with help, build-version reporting, executable-relative roots, and informational `upgrade`/`update` commands.
- Add Make commands for local installation, atomic rebuilds, formatting, linting, and isolated unit and race tests, plus dependency-only `make setup` and optional `VERBOSE=1` setup/build diagnostics. Installation initializes nonsecret state and publishes executables under lifecycle locking; rebuilding reports when the service needs an explicit restart. `make clean` delegates to uninstall with purge disabled; both commands currently fail explicitly without changing files until uninstall is implemented.
- Add strict nonsecret connection profiles with validated scopes, transport settings, resource limits, and atomic persistence.
- Add AES-256-GCM credential storage backed by one macOS Keychain item per installation, with durable encryption accounting and recoverable credential cleanup.
- Add interactive and scripted `db add`, `db edit`, `db remove`/`rm`, and `db list`/`ls`, with nonsecret previews, default-No confirmation, cancellation without saved changes, and versioned JSON listing.
- Add password and credential input through stdin, explicit secret preservation/clearing, and saved scope, TLS, SSH, proxy, and query-limit settings without opening database connections.
- Add internal PostgreSQL direct and verified TLS connections, read-only role checks, scoped catalog pagination and table descriptions, bounded connection pools, and redacted diagnostics.
- Add a default-deny internal SQL compiler for a finite SELECT subset, including joins, grouping, audited aggregates, CTEs, subqueries, and typed parameters, with PostgreSQL 16/18 catalog verification and parameterized SQL emission. Verify live catalog metadata by semantic identity, ignoring incidental row IDs and numeric planner estimates while preserving implementation and safety checks.
- Add bounded read-only PostgreSQL query execution to the internal driver, with fresh authorization checks, exact result codecs, row and byte limits, and cancellation cleanup.
- Add SSH password/imported-key and SOCKS5 routes with optional verified TLS, remote database DNS, bounded cancellation, and interactive `db add/edit --ssh-enroll` fingerprint confirmation with strict host-key pinning.
- Add interactive, searchable `db scope` with lazy catalog pages, exact scripted scope replacement, and confirmation that waits for existing database work to finish.
- Add staged `db test [alias]` diagnostics with versioned JSON, safe errors, shared read-only policy checks, and continued testing after individual connection failures.
- Add opt-in, isolated Docker integration checks for PostgreSQL 16 and 18 through `make test-integration`; MCP delivery and agent registration remain unavailable.
- Add `mcp start`, `mcp stop`, and passive `mcp status` with versioned JSON, isolated launchd jobs, verified service identity, bounded readiness and shutdown, and profile reload handling. MCP delivery and agent registration remain pending.
