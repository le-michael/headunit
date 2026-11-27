# AGENTS.md

`Headunit` is a private, self-hosted OpenSubsonic music streaming server built in Go with embedded SQLite and Docker Compose.

## Commands

Run from the repository root once the Go module and Docker setup are in place:

- Build static binary: `CGO_ENABLED=0 go build ./cmd/headunit`
- Run full test suite: `go test -race ./...`
- Run Docker Compose E2E suite: `./scripts/ci/docker-e2e.sh` (runs `go test -tags=e2e -v -count=1 ./e2e`)
- Run a single test: `go test -race -run TestName ./internal/<pkg>`
- Static analysis: `go vet ./...` and `go vet -tags=e2e ./...`
- Format code: `gofmt -w .`
- Run locally via Docker Compose: `docker compose up -d --build`

## Architecture & Boundaries

- **Strict scope discipline:** Keep changes minimal; do not add unrequested features, endpoints, UI, or third-party dependencies unless explicitly asked.
- **Pure-Go build:** Keep the binary `CGO_ENABLED=0` compatible (no CGO dependencies).
- **Library decoupling:** Never mirror the host directory structure in the in-app library. Treat `/music` as read-only (`:ro`) and store all server state under `/data`.
- **Canonical API tracker:** Reference `docs/opensubsonic_compatibility.md` as the source of truth for OpenSubsonic coverage. Update its status rows and summary counts whenever endpoints, stubs, error codes, or protocol features change.
- **Restricted files:** Never commit `.env`, secrets, or local database files (`*.db`, `*.db-wal`, `*.db-shm`).

## Go Style & Readability

Follow the [Google Go Style Guide](https://google.github.io/styleguide/go/) (`/guide`, `/decisions`, and `/best-practices`), prioritizing clarity, simplicity, concision, maintainability, and consistency:

- **Formatting:** Always format Go code with `gofmt`.
- **Naming:** Use `MixedCaps` (avoid underscores outside `*_test.go`), use consistent initialisms (`ID`, `HTTP`, `URL`, `SQL`), avoid stuttering across package, receiver, and function names (e.g., `store.Open`, not `store.OpenStore`), and keep receiver names short and consistent.
- **Error handling & control flow:** Handle errors explicitly where they occur, wrap with context (`fmt.Errorf("...: %w", err)`), indent error paths with early returns to keep the happy path unindented, and never use `panic` for normal error handling.
- **Error definitions:** Use inline `errors.New` / `fmt.Errorf` for preconditions, config validation, and internal failures callers only log or propagate. Export sentinel errors (`var Err... = errors.New(...)`)—or wrap stdlib sentinels like `os.ErrClosed`/`os.ErrPermission`—only when callers branch on them via `errors.Is`, and define exported error structs only when callers inspect structured fields via `errors.As`.
- **Contexts & interfaces:** Pass `context.Context` as the first parameter (`ctx`) and never store it in a struct. Define interfaces at the consumer rather than the producer when practical.
- **File size & pragmatic abstraction:** Avoid god files; split responsibilities and introduce abstractions when they clarify boundaries or rein in growing complexity. Do not over-abstract—only extract logic into separate structs or files when it genuinely makes sense and keeps changes easy to review.
- **Comments:** Write doc comments on exported symbols as complete sentences starting with the symbol's name; explain *why*, not *what*.
- **Tests:** Prefer table-driven tests, mark helpers with `t.Helper()`, and emit clear `got`/`want` failure messages (avoid third-party assertion frameworks).

## Testing Instructions

- **Go-native tests only:** Write all test logic, fixture generation, and assertions in Go (`*_test.go`), using build tags (such as `//go:build e2e`) when external infrastructure like Docker is required. Keep shell scripts thin and restricted to container/process orchestration or CI wiring—never embed Python or other scripting languages in shell scripts or workflows for test fixtures, HTTP checks, or assertions.
- Add or update unit and integration tests for all behavioral changes whenever possible.
- Run `go test -race ./...`, `go vet ./...`, and `go vet -tags=e2e ./...` before committing or opening a PR; fix all failures before requesting review.
- For HTTP/streaming changes, verify both XML (default) and JSON (`f=json`) responses as well as HTTP byte-range (`206`, `416`, `HEAD`) behavior where applicable.

## PR Instructions

- **Always use PRs:** Never push changes directly to `main`; create a feature branch and open a pull request.
- **Small, stackable PRs:** Keep each PR small and focused on a single concern. For larger features, break work into a stack of small PRs for review.
- **Conventional Commits:** Use [Conventional Commits](https://www.conventionalcommits.org/) for all commit messages and PR titles (e.g., `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
- **PR description:** Always include a simple, plain-language `Summary` section explaining why the change is being made (accessible to any reader, without referencing "ELI5"), followed by a clear breakdown of the delta being applied.
- **Pre-PR checklist:** Ensure tests pass, scope is minimal, and `docs/opensubsonic_compatibility.md` is updated if OpenSubsonic compatibility changed.
