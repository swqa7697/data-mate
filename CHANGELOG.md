# Changelog

## [Unreleased]

### Added

- Add scoped `list_objects` and `describe_object` MCP tools for routine source, enum and other type details, and sequence configuration; expand table descriptions with defaults, indexes, constraints, triggers, view definitions, and row-security policies without evaluating stored expressions.
- Add `db describe [alias]` with an interactive profile picker and versioned `--json` output to list accessible schemas and readable tables/views, mark allowed and excluded schemas, and show the saved scope policy; fail clearly when complete results exceed catalog, byte, or timeout limits.

### Changed

- Restrict explicit agent SQL calls to core PostgreSQL routines, retaining functions such as `enum_range` while rejecting application and extension routine calls; preserve indirect reads through permitted views, operators, casts, and policies.
- Replace schema/table scope selection with schema-only whitelist and blacklist modes, defaulting new connections to all accessible schemas. Add `--exclude-schema` and a colored picker with immediate checkbox toggling, one-key toggle-all, and mode switching that preserves selections; old scope formats require profile recreation.
- Show one `PASS` or `FAIL` summary per connection in `db test`, with details only for failed checks and green/red terminal status labels that honor `NO_COLOR`; preserve plain redirected output and complete JSON diagnostics.

### Fixed

- Remove recorded installation artifacts during uninstall even after edits, replacement, permission changes, or filesystem device renumbering; preserve unrelated files and symlink targets, and report the path when cleanup fails.

## [0.1.1] - 2026-09-25

### Fixed

- Prevent installation and upgrades from failing on an additional online notarization lookup; retain checksum and pinned publisher-signature verification, release notarization, and macOS security enforcement.

## [0.1.0] - 2026-09-25

### Added

- Add a checkout-local macOS Apple Silicon CLI with help, build-version reporting, a separate `.dev/data-mate/` data root beside `.dev/bin/data-mate`, and executable-relative root discovery.
- Add Make commands for local installation, atomic rebuilds, formatting, linting, and isolated unit and race tests, plus dependency-only `make setup` and optional `VERBOSE=1` setup/build diagnostics. Installation initializes nonsecret state and publishes executables under lifecycle locking; rebuilding reports when the service needs an explicit restart. `make clean` delegates to uninstall with purge disabled.
- Add strict nonsecret connection profiles with validated scopes, transport settings, resource limits, and atomic SQLite persistence shared with encrypted credential records.
- Add Tink AES-256-GCM credential bundles backed by one macOS Keychain keyset per installation, with authenticated connection binding, durable encryption accounting, atomic updates, and recoverable initialization and purge. The persistent service retains its unlocked keyset so ordinary connection saves require no further Keychain access.
- Add interactive and scripted `db add`, `db edit`, `db remove`/`rm`, and `db list`/`ls`, with nonsecret previews, default-No confirmation, cancellation before submission without saved changes, and versioned JSON listing. Confirmed changes use a private management service; listing and previews remain passive.
- Add password and credential input through stdin, explicit secret preservation/clearing, and saved scope, TLS, SSH, proxy, and query-limit settings without opening database connections.
- Add internal PostgreSQL direct and verified TLS connections, read-only transactions, scoped catalog pagination and table descriptions, bounded connection pools, and redacted diagnostics. Metadata lists readable relations, including those accessible through column-level SELECT grants, and reports actual types without support flags.
- Add bounded PostgreSQL read queries through an operator-managed read-only account and read-only transactions. A small guard checks statement kind and directly referenced relation scope while PostgreSQL enforces privileges and query semantics, including views, arrays, enums, custom types, generated columns, windows, and recursive CTEs. Queries accept JSON value arrays as parameters and return structured array results or PostgreSQL text fallbacks for other types, with fresh authorization checks, row and byte limits, cancellation cleanup, and distinct write, permission, and query errors with safe SQLSTATE values. The default timeout is 60 seconds, configurable up to five minutes, with up to eight connections per profile.
- Add SSH password/imported-key and SOCKS5 routes with optional verified TLS, remote database DNS, bounded cancellation, and interactive `db add/edit --ssh-enroll` fingerprint confirmation with strict host-key pinning.
- Add interactive, searchable `db scope` with lazy catalog pages, exact scripted scope replacement, and confirmation that waits for existing database work to finish.
- Add staged `db test [alias]` diagnostics with versioned JSON, safe errors, shared service-owned credential access, a `read_only` transaction check, and continued testing after individual connection failures.
- Add opt-in, isolated Docker integration checks for PostgreSQL 16 and 18 through `make test-integration`.
- Add `mcp start`, `mcp stop`, and passive `mcp status` with versioned JSON, a shared first-wins launchd service slot across development and production, verified service identity, bounded readiness and shutdown, and profile reload handling. Management-only startup keeps agent access disabled until `mcp start`; status reports MCP enablement, keyset availability, and a blocking installation owner separately.
- Add four read-only MCP tools and an authenticated stdio bridge, with strict schemas, scoped metadata, shared query authorization, bounded payloads, cancellation, and redacted errors. Each session accepts up to 16 concurrent requests; shared admission allows 32 active operations and 128 waiters with a 60-second queue deadline.
- Add automatic user-scope Codex/Claude registration after service readiness, passive per-agent status, conflict preservation, and recoverable ownership records that retain exact agent configuration locations for cleanup; partial registration failures keep the service available.
- Add ownership-checked uninstall with credential-preserving reinstall, explicit `PURGE=1` exact-key cleanup, lifecycle serialization, interruption recovery and retained cleanup helpers on failure. Purge removes the empty `.dev` directory, including on retries, while preserving unrelated files and symlinks.
- Add standalone signed/notarized macOS 15+ Apple Silicon installation at a fixed per-user root, verified stable-release `upgrade`/`update`, recoverable publication, and production `uninstall` with credential retention or explicit `--purge` cleanup. Release bootstrap availability begins with the first stable publication.
- Add passive Bash/zsh command and connection-alias completion, owned shell activation with `--no-shell` opt-out, and preservation of unrelated startup-file content. Development retains root overrides and uses Make for installation and cleanup.
