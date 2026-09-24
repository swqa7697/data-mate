# Changelog

## [Unreleased]

### Added

- Add a checkout-local macOS Apple Silicon CLI with help, build-version reporting, executable-relative roots, and informational `upgrade`/`update` commands.
- Add Make commands for local installation, atomic rebuilds, formatting, linting, and isolated unit and race tests, plus dependency-only `make setup` and optional `VERBOSE=1` setup/build diagnostics. Installation initializes nonsecret state and publishes executables under lifecycle locking; rebuilding reports when the service needs an explicit restart. `make clean` delegates to uninstall with purge disabled.
- Add strict nonsecret connection profiles with validated scopes, transport settings, resource limits, and atomic persistence.
- Add AES-256-GCM credential storage backed by one macOS Keychain item per installation, with durable encryption accounting and recoverable credential cleanup.
- Add interactive and scripted `db add`, `db edit`, `db remove`/`rm`, and `db list`/`ls`, with nonsecret previews, default-No confirmation, cancellation without saved changes, and versioned JSON listing.
- Add password and credential input through stdin, explicit secret preservation/clearing, and saved scope, TLS, SSH, proxy, and query-limit settings without opening database connections.
- Add internal PostgreSQL direct and verified TLS connections, read-only transactions, scoped catalog pagination and table descriptions, bounded connection pools, and redacted diagnostics.
- Add bounded read-only PostgreSQL query execution to the internal driver, with fresh authorization checks, exact result codecs, row and byte limits, and cancellation cleanup.
- Add SSH password/imported-key and SOCKS5 routes with optional verified TLS, remote database DNS, bounded cancellation, and interactive `db add/edit --ssh-enroll` fingerprint confirmation with strict host-key pinning.
- Add interactive, searchable `db scope` with lazy catalog pages, exact scripted scope replacement, and confirmation that waits for existing database work to finish.
- Add staged `db test [alias]` diagnostics with versioned JSON, safe errors, read-only transaction checks, and continued testing after individual connection failures.
- Add opt-in, isolated Docker integration checks for PostgreSQL 16 and 18 through `make test-integration`.
- Add `mcp start`, `mcp stop`, and passive `mcp status` with versioned JSON, isolated launchd jobs, verified service identity, bounded readiness and shutdown, and profile reload handling.
- Add four read-only MCP tools and an authenticated stdio bridge, with strict schemas, scoped metadata, shared query authorization, bounded sessions and payloads, cancellation, and redacted errors.
- Add automatic user-scope Codex/Claude registration after service readiness, passive per-agent status, conflict preservation, and recoverable ownership records; partial registration failures keep the service available.
- Add ownership-checked uninstall with credential-preserving reinstall, explicit `PURGE=1` exact-key cleanup, lifecycle serialization, interruption recovery and retained cleanup helpers on failure.

### Changed

- Raise the default query timeout to 60 seconds (configurable up to five minutes), allow eight database connections per profile and 16 concurrent requests per MCP session, and expand shared admission to 32 active operations and 128 waiters with a 60-second queue deadline.
- Allow ordinary PostgreSQL read queries, including views, arrays, enums, custom types, generated columns, windows and recursive CTEs, within configured direct-relation scope.
- Replace exhaustive SQL/catalog and privilege audits with a small read-query guard and PostgreSQL read-only transactions; require an operator-managed read-only account.
- Use simple JSON value arrays for query parameters, structured array results and PostgreSQL text fallbacks for other types.
- Remove metadata support flags, use `read_only` diagnostics, and distinguish write, permission and query errors with safe SQLSTATE values.
- List readable relations without per-table inspection, including tables accessible through column-level SELECT grants.
