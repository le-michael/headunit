# AGENTS.md

`Headunit` is a private, self-hosted OpenSubsonic music streaming server built in Go with embedded SQLite and Docker Compose.

## Commands

Run from the repository root once the Go module and Docker setup are in place:

- Build static binary: `CGO_ENABLED=0 go build ./cmd/headunit`
- Run full test suite: `go test -race ./...`
- Run a single test: `go test -race -run TestName ./internal/<pkg>`
- Static analysis: `go vet ./...`
- Run locally via Docker Compose: `docker compose up -d --build`

## Architecture & Boundaries

- **Strict scope discipline:** Keep changes minimal; do not add unrequested features, endpoints, UI, or third-party dependencies unless explicitly asked.
- **Pure-Go build:** Keep the binary `CGO_ENABLED=0` compatible (no CGO dependencies).
- **Library decoupling:** Never mirror the host directory structure in the in-app library. Treat `/music` as read-only (`:ro`) and store all server state under `/data`.
- **Canonical API tracker:** Reference `docs/opensubsonic_compatibility.md` as the source of truth for OpenSubsonic coverage. Update its status rows and summary counts whenever endpoints, stubs, error codes, or protocol features change.
- **Restricted files:** Never commit `.env`, secrets, or local database files (`*.db`, `*.db-wal`, `*.db-shm`).

## Testing Instructions

- Add or update unit and integration tests for all behavioral changes whenever possible.
- Run `go test -race ./...` and `go vet ./...` before committing or opening a PR; fix all failures before requesting review.
- For HTTP/streaming changes, verify both XML (default) and JSON (`f=json`) responses as well as HTTP byte-range (`206`, `416`, `HEAD`) behavior where applicable.

## PR Instructions

- **Always use PRs:** Never push changes directly to `main`; create a feature branch and open a pull request.
- **Small, stackable PRs:** Keep each PR small and focused on a single concern. For larger features, break work into a stack of small PRs for review.
- **Conventional Commits:** Use [Conventional Commits](https://www.conventionalcommits.org/) for all commit messages and PR titles (e.g., `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
- **Pre-PR checklist:** Ensure tests pass, scope is minimal, and `docs/opensubsonic_compatibility.md` is updated if OpenSubsonic compatibility changed.
