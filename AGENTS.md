# Data Mate development

- Read `specs/PRD.md` for scope and `specs/DESIGN.md` for contracts. Implement one package from the local implementation plan at a time; record validation before advancing.
- Target one Go executable for macOS on Apple Silicon. Install only under this checkout's `.dev`. Keep `VERSION` the sole checked-in application version.
- Keep CLI, configuration, vault, PostgreSQL policy, transport, service, MCP, and agent registration boundaries separate. Use concrete implementations until an actual external seam needs substitution.
- Unfinished operations must fail clearly. Never bypass authorization to make a stub succeed. Only `upgrade`/`update` are informational success stubs.
- Profiles are nonsecret. Do not persist plaintext credentials or expose secrets, SQL parameters, or upstream diagnostics in output. Validate bounded input before use; SQL parsing alone never authorizes execution.
- Tests use isolated temporary roots and synthetic providers. Docker integration is explicit and excluded from CI; native Keychain and agent checks require isolated opt-in fixtures.
- Run `make format-check`, `make lint`, `make test`, `make test-race`, and `make build` for a completed package. Record unavailable checks honestly; workflow configuration is not hosted CI evidence.
- Retain plans, implementation status, and evidence in ignored `.misc`. `.tmp` is developer-managed. Keep README and design contracts synchronized with implemented behavior.
