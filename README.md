# Data Mate

A checkout-local Go CLI for sharing PostgreSQL connections with terminal agents
through a read-only MCP service on macOS with Apple Silicon.

**Current implementation: P0 foundation.** Help, version, root resolution, strict
profile/schema contracts, and developer tooling are implemented. Database CRUD,
credentials, query execution, MCP service, and agent registration are not ready.
Those commands fail without creating runtime state. `upgrade` and `update`
explain how to rebuild locally and make no network request.

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

The installed binary resolves its root from its executable location, so it works
from another working directory. An absolute `--root` overrides that location.
Each checkout has its own `.dev`; installation creates only its private `bin`
directory and executable. Persistent installation identity and locking arrive in
P1/P8. Builds replace the executable atomically. No service, agent registration,
or Keychain item is created during installation.

| Command | Behavior |
| --- | --- |
| `make help` | List all development commands; default target |
| `make install` | Download pinned dependencies/tools and build the local binary |
| `make build` | Rebuild and atomically replace the local binary |
| `make dev ARGS="..."` | Run that binary with the checkout's absolute root |
| `make clean` | Remove reserved `.build` output; preserve `.dev` and evidence |
| `make format`, `make tidy` | Format Go and shell sources |
| `make format-check` | Check formatting without changing files |
| `make lint` | Run go vet and pinned staticcheck |
| `make test` | Run isolated unit/regression tests, including cgo dependency compilation |
| `make test-race` | Run the suite with race detection |
| `make test-integration DB_DRIVER=postgres DB_IMAGE=postgres:16` | Not ready until P3; fails explicitly; excluded from CI |
| `make uninstall`, `make uninstall PURGE=1` | Not ready until P11; fail without changing files |

The normal validation order is `make format-check`, `make lint`, `make test`,
`make test-race`, and `make build`. After installation, tests disable Go module
network access and use disposable roots. They do not connect to a real database,
read/write Keychain items, or alter agent configuration. The CI workflow uses
`macos-15`, records its actual architecture/toolchain, and runs these local gates.
A workflow file alone does not establish a successful hosted run.

Scripts under `scripts/` back the matching Make targets; `common.sh` supplies
checkout paths, target settings, dependency checks, and directory validation.
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
