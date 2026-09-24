# Data Mate development guidelines

## Scope and sources of truth

- Data Mate is one Go executable for macOS on Apple Silicon, installed in the checkout's `.dev`, exposing read-only PostgreSQL access through MCP. PostgreSQL 16 and 18 are the integration test matrix.
- Read [PRD](specs/PRD.md) for product scope, [DESIGN](specs/DESIGN.md) for technical contracts, and [README](README.md) for implemented commands and setup. Keep them synchronized when behavior changes.
- Keep retained development artifacts, such as test results, diagnostic logs and performance measurements, in ignored `.misc`. `.tmp` is developer-managed; read supplied reference material there without modifying it.
- [CLAUDE.md](CLAUDE.md) points here. Maintain one shared set of project guidelines.

## Architecture and ownership

| Location | Responsibility |
| --- | --- |
| `cmd/data-mate`, `internal/cli` | Process entry, command parsing, forms, output and exit contracts |
| `internal/config`, `internal/contracts` | Nonsecret profiles, roots, revisions, bounded decoding and public schemas |
| `internal/vault` | Encrypted credentials and the native OS key-provider boundary |
| `internal/database`, `internal/database/postgres` | Driver operations, catalog access, SQL policy and codecs |
| `internal/transport` | Explicit direct/TLS/SSH/SOCKS5 connection paths |
| `internal/service`, `internal/mcp` | Lifecycle, admission, sessions, tool dispatch and the stdio bridge |
| `internal/agent` | Codex/Claude registration and ownership verification |
| `internal/devtools`, `scripts`, `.github/workflows` | Developer command regressions, tooling and CI |

- Keep database and authorization behavior shared across CLI/MCP callers. Agent adapters own registration and compatibility; the bridge must not acquire vault or driver responsibilities.
- Prefer concrete packages and small functions. Introduce interfaces only at real external seams used by callers/tests; do not add a plugin framework or universal query abstraction in advance.
- Keep deterministic parsing/validation separate from filesystem, subprocess, network and key-store effects. Follow DESIGN for lock ordering, publication and cleanup contracts.

## Go and shell conventions

- Use the Go standard library for JSON, crypto, logging, filesystem and process primitives where suitable. Keep dependencies and developer tools pinned in `go.mod`/`go.sum`; avoid floating versions in scripts.
- Follow established Go naming and `gofmt`; document exported contracts and non-obvious invariants. Return errors instead of panicking on user input or operational failures.
- Propagate `context.Context` through blocking operations. Bound concurrency and resource use; make ownership, cancellation and cleanup explicit for goroutines, locks, connections and subprocesses.
- Wrap internal errors where useful, but translate them into safe public diagnostics at CLI/MCP boundaries. Do not expose secret-bearing upstream errors.
- Use Bash with `set -euo pipefail`, quoted expansions and argument arrays. Format scripts with the pinned shfmt through `make format`; keep reusable command logic under `scripts/`.
- Make focused changes, remove dead code, and share helpers only when they express the same responsibility. Avoid speculative abstractions and unrelated cleanup.

## Security and data contracts

- Profiles contain nonsecret settings only. Never persist plaintext credentials or expose them through command arguments, previews, logs or MCP responses. Use synthetic secret values in tests; never commit real credentials.
- Validate input size, structure, duplicate keys, versions and unknown fields before use. Saved scope, database privileges and supported SQL semantics must all authorize an operation.
- PostgreSQL parsing is not authorization. Preserve the typed default-deny policy and resource bounds; reject unsupported SQL explicitly.
- Use explicit connection/transport configuration. Do not inherit ambient database credentials, SSH agents or proxy settings as fallback behavior.
- Resolve installation paths independently of the caller's working directory. Verify ownership and symlink safety before mutating owned state; preserve unrelated files and external certificates/key sources.
- Keep stdout reserved for requested machine/protocol output where applicable. Report diagnostics to stderr and preserve the documented success, operational failure, invalid input and cancellation exit codes.

## Development commands and validation

Use Make as the developer entry point; `make help` lists the available targets.

| Command | Purpose |
| --- | --- |
| `make setup` | Prepare pinned dependencies/tools without building the application |
| `make install` | Download pinned Go dependencies/tools and install `.dev/bin/data-mate` |
| `make build` | Rebuild and atomically replace the development executable |
| `make dev ARGS="help"` | Invoke the installed executable with this checkout's root |
| `make format` / `make tidy` | Format Go and Bash; `tidy` aliases formatting |
| `make format-check` | Check formatting without writing files |
| `make lint` | Run go vet and pinned staticcheck |
| `make test` | Run the offline unit/regression suite after dependencies are installed |
| `make test-race` | Run the same suite with Go's race detector |
| `make test-integration DB_DRIVER=postgres` | Run owned Docker fixtures for both PostgreSQL 16 and 18; excluded from CI |
| `make clean` | Alias for uninstall with purge disabled; preserves profiles and credentials |
| `make uninstall` / `make uninstall PURGE=1` | Remove owned installation resources; purge also removes profiles and credentials |

- Use the Go toolchain pinned in `go.mod` and `scripts/common.sh`, native cgo and the macOS SDK. Dependency checks must provide guidance rather than silently install system software.
- Format changed Go/shell code with `make format`. After code changes, run this checklist in order:
  - [ ] `make format-check`
  - [ ] `make lint`
  - [ ] `make test`
  - [ ] `make test-race`
  - [ ] `make build`
- For a documentation-only change, verify references, commands and `git diff --check`; do not add tests or rerun application suites without a behavioral reason.
- CI separates Format and lint, Test and build, and Race tests on `macos-15`, with `make setup` in each job. Preserve check coverage and test isolation when changing job structure.
- Report checks run, results and limitations in the task response. Skipped checks are unverified; claim hosted CI success only after an actual successful run.

## Test suite design

- Keep `*_test.go` beside the owning Go package. Test observable behavior at the highest layer that owns the contract: CLI command scenarios, profile decoding, public driver/service operations, or installed-binary subprocesses in `internal/devtools`.
- Keep most regressions deterministic and local. Use focused unit tests where an algorithm or security invariant cannot be exercised clearly through an existing scenario; avoid duplicating the same assertions across layers.
- Use `t.TempDir`, `t.Cleanup`, synthetic peers and controlled subprocess environments. Default tests must not access real credentials, user agent configuration, or external databases; subprocess tests must propagate failures and clean owned resources.
- Reuse fakes and harnesses in package-local test helper files, marking helpers with `t.Helper`. Extract cross-package support only when actual reuse requires a shared internal test-support package. Never copy a fake into each test file or couple tests to another scenario's setup/state.
- Keep reusable fixtures in the owning package's `testdata`. Decode/execute them through real contracts; static fixture examples alone do not prove database semantics or authorization.
- Use `t.Parallel` only for independent fixtures. Tests changing process-global environment, working directory, or shared resources must remain serialized or move those effects into isolated subprocesses.
- Prefer deterministic synchronization and context deadlines to sleeps for concurrency tests. Exercise cross-process locking separately from goroutine races when the contract spans processes.
- For PostgreSQL behavior changes, run `make test-integration DB_DRIVER=postgres DB_IMAGE=postgres:16` and the corresponding `postgres:18` invocation. Docker fixtures must be owned and isolated; never redirect them to a user's database or enable them in CI.
- Native Keychain/launchd/agent checks need explicit opt-in and isolated identities; follow the opt-in commands in README. Environment-guarded skips must name the missing prerequisite and be reported as unverified.
- Bound fuzz input size and run time, retain meaningful regression seeds, and keep extended fuzzing outside normal CI. Existing seeds run with the ordinary Go suite.

## Test growth rules

**Regression-first — mandatory decision ladder.** Before writing ANY new test case, first analyze the existing regression tests covering the changed behavior. Follow these steps in order and stop at the first applicable step; do not skip directly to adding a new test:

1. Existing regression coverage verifies the change: add nothing.
2. An existing regression can reasonably cover the change: extend that scenario or its curated cases; do not create a separate test.
3. Only when neither applies—no existing regression covers the behavior and extending one would be unsuitable—may a new test case be added at the highest owning layer. Explain why the first two steps do not apply.

- Every new case must protect a distinct failure mode or contract. Document the bug story in the scenario name/comment; a new file must own a coherent behavior, not a single-assertion micro-test.
- Do not test constants, enum members, getters or struct defaults in isolation. Verify consequential behavior through decoding, execution or public outcomes; security-relevant default-limit enforcement is a behavior, not a field-value snapshot.
- Do not assert static UI/help prose, source-file substrings, documentation text, module manifests or SQL text files. Verify CLI exit/stream/redaction contracts and actual database outcomes instead of incidental wording or implementation layout.
- Keep duplicated documentation/example/source text consistent through review, not parity/diff tests. Existing legacy checks are not a template for new tests; changing them requires a justified scoped change.
- Use small curated table-driven tests and named `t.Run` subtests for boundaries and representative errors. Avoid exhaustive cross-products. A required large input/output corpus belongs in one loop-bodied test with a case identifier in each failure; preserve all distinct security cases.
- `t.Run` creates separately reported executions; moving a table into subtests does not reduce suite size. Loops still execute every case and cost time, so do not use reporting structure to hide growth.
- Do not park failures behind unconditional `t.Skip`, build tags or test-name filters. Runtime `t.Skip` is allowed only for a genuine environment/opt-in guard; required skipped checks are not passes.
- Keep ordinary regression assertions independent of performance timing. For material suite growth, explain the behavior protected and measure runtime impact, including expanded loop corpora and fixture setup costs.
- Do not inflate tests to satisfy a case-count/coverage target or delete required security coverage to meet a budget.

## Performance evidence

- For performance changes, measure before/after on the same toolchain, fixture, machine and cache conditions. Use targeted Go benchmarks or command/fixture measurements as appropriate; Go test wall time includes build/setup work, so distinguish orchestration from the operation being optimized.
- Choose trial count dynamically with at least three independent trials and at most five minutes per before/after phase. If these requirements cannot both be met, record insufficient evidence rather than claiming improvement.
- Report sample count, mean, median, standard deviation, min/max and p95/p99, noting unstable tail estimates with small samples. Use Welch's t-test at alpha 0.05; label confidence by degrees of freedom: high >=29, moderate 10–28, low <10.
- Keep raw results, source revision, commands, environment and limitations in `.misc`; avoid unsupported speedup claims.

## Git, review and documentation

- Develop on `dev` or a focused topic branch; never commit directly to `main` or `master`. Preserve unrelated staged and working-tree changes. Staging, committing and pushing each require the user's explicit authorization.
- Keep commits focused and use English Conventional Commit subjects: `type(scope): imperative summary`, lowercase subject, no trailing period, at most 72 characters. Use concise bullet points for a body when useful.

| Types | Use |
| --- | --- |
| `feat`, `fix` | New behavior; defect correction |
| `refactor`, `perf` | Behavior-preserving restructuring; measured performance improvement |
| `test`, `build`, `ci` | Test coverage; build/dependency tooling; CI workflows |
| `docs`, `style`, `chore`, `revert` | Documentation; formatting; maintenance; reverting a change |

Examples: `feat(vault): persist encrypted credential bundles`; `fix(config): reject duplicate profile identities`.

- PR descriptions explain the problem, resulting behavior, validation and remaining limitations. Test additions/material growth must explain the decision ladder and cost; do not present planned checks as completed results.
- Keep `VERSION` the sole checked-in application version source. Build metadata may add revision/dirty state; do not duplicate version literals elsewhere.
- Update README for changed setup, commands and availability, and DESIGN for changed contracts. Keep this guide focused on durable development rules.
- When maintaining `CHANGELOG.md`, use Keep a Changelog with user-visible entries under `[Unreleased]` in the applicable Added/Changed/Deprecated/Removed/Fixed/Security category. Preserve spacing, omit redundant empty lines, and avoid entries for internal-only refactors or formatting.
