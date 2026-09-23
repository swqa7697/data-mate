# Data Mate

A checkout-local Go CLI for sharing PostgreSQL connections with terminal agents
through a read-only MCP service on macOS with Apple Silicon.

**Current implementation: P1 persistence and credential vault.** Help, version,
root resolution, strict profile/schema contracts, developer tooling, and the
internal profile/vault store are implemented. The store uses verified private
files, cross-process leases, atomic publication, AES-256-GCM, durable encryption
accounting, and one native Keychain item per installation. CLI connection CRUD
arrives in P2; query execution, MCP service, and agent registration remain later
packages. Those unfinished commands fail without creating runtime state.
`upgrade` and `update` explain how to rebuild locally and make no network request.

Install Go 1.27.1 and Apple's Command Line Tools (`xcode-select --install`). Then:

```bash
make install
make dev ARGS="help"
make dev ARGS="version"
```

`make install` downloads the pinned Go modules, shfmt, and staticcheck and builds
`.dev/bin/data-mate`. Dependency checks provide installation guidance; they never
install system packages. Go 1.27.1 is selected explicitly by the scripts. Native
cgo and the macOS SDK are required. The binary reports the application version
from `VERSION`, plus revision and working-tree state when Git metadata is present.

Use `make setup` to prepare the same dependencies/tools without compiling the
application or creating `.dev`. Add `VERBOSE=1` to `make setup`, `make install`, or
`make build` to show Go download/build commands when those operations run.

The installed binary resolves its root from its executable location, so it works
from another working directory. An absolute `--root` overrides that location.
Each checkout has its own `.dev`; installation creates only its private `bin`
directory and executable. The internal store initializes persistent identity and
state locks when opened for confirmed work; install/build do not initialize that
store. Service lifecycle integration arrives in P8. Builds replace the executable
atomically. No service, agent registration, or Keychain item is created during installation.

| Command | Behavior |
| --- | --- |
| `make help` | List all development commands; default target |
| `make setup` | Prepare pinned dependencies/tools without building the application |
| `make install` | Download pinned dependencies/tools and build the local binary |
| `make build` | Rebuild and atomically replace the local binary |
| `make dev ARGS="..."` | Run that binary with the checkout's absolute root |
| `make clean` | Alias for uninstall with purge disabled; not ready until P11, fails without changing files |
| `make format`, `make tidy` | Format Go and shell sources |
| `make format-check` | Check formatting without changing files |
| `make lint` | Run go vet and pinned staticcheck |
| `make test` | Run isolated unit/regression tests, including cgo dependency compilation |
| `make test-race` | Run the suite with race detection |
| `make test-integration DB_DRIVER=postgres DB_IMAGE=postgres:16` | Not ready until P3; fails explicitly; excluded from CI |
| `make uninstall`, `make uninstall PURGE=1` | Not ready until P11; fail without changing files |

CI runs `make setup` in each job before its checks; installation is exercised by
the regression suite and the Test and build job ends with `make build`.

The normal validation order is `make format-check`, `make lint`, `make test`,
`make test-race`, and `make build`. After installation, tests disable Go module
network access and use disposable roots. They do not connect to a real database,
read/write Keychain items, or alter agent configuration. The CI workflow uses
`macos-15`, records its actual architecture/toolchain, and runs these local gates.
A workflow file alone does not establish a successful hosted run.

P1's explicit native check uses only synthetic credentials, a random temporary
root, one exact Keychain account, and a temporary launchd job:

```bash
DATA_MATE_NATIVE_TEST=1 make test
```

This opt-in requires an unlocked macOS user Keychain and a GUI launchd session.
The default suite skips this native gate and uses fake providers. The native
fixture also creates its own temporary Keychain for locked/denied/unavailable
checks; it never locks the user's Keychain. A rebuilt ad-hoc executable may need
new OS authorization: denial preserves the vault and never resets its key.
Unattended access reports denial rather than displaying UI. If fixture cleanup
fails, the test reports and retains its exact helper/root for retry.

Vault writes reserve one of a maximum of 1,000,000 encryptions durably before
sealing. Failed publication consumes its reservation. Profile edits publish a
fresh encrypted bundle before its reference; deletion publishes profiles before
credential cleanup and reports partial completion if cleanup fails. The internal
purge coordinator removes the exact key before its accounting, retains retry
identity, and blocks ordinary access once purge begins. Full `uninstall`/`clean`
integration remains P11. Do not restore old vault/accounting backups or move
Keychain items between stores; recovery is nonsecret profile import and credential
re-entry in a fresh installation namespace.

Scripts under `scripts/` back the matching Make targets; `common.sh` supplies
checkout paths, target settings, dependency checks, and directory validation.
`install.sh` runs `setup.sh` before `build.sh`.
The `clean` target delegates to `uninstall` with `PURGE=0`, overriding any supplied
`PURGE` value. Use `make uninstall PURGE=1` to explicitly request purge.
`dev` invokes the binary directly and `test-race` adds `-race` to `test.sh`.

Profile and MCP contracts are embedded in `internal/contracts/schemas`, with
examples in `internal/contracts/testdata`. Profiles reject unknown fields,
duplicate keys/identities/references, malformed scopes, unsupported transports,
and excess limits. No plaintext secret field belongs in a profile. Selected
empty scopes expose nothing; missing scope is invalid. New-profile creation in
P2 will explicitly choose all. PostgreSQL codec/signature fixture formats are
under `internal/database/postgres/testdata`; these are future acceptance inputs,
not a working SQL compiler or authorization policy.

See [PRD](specs/PRD.md) and [technical design](specs/DESIGN.md) for intended product
behavior. Local implementation progress and evidence live in ignored `.misc`.
