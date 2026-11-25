# AGENTS.md

`Headunit` is a private OpenSubsonic music streaming server (Go, SQLite, Docker Compose).

## Workflow Guidelines

1. **Pull Requests:** Create PRs for all changes rather than pushing directly to `main`.
2. **Small, Focused PRs:** Keep PRs small and easy to review. For larger work, split changes into a stack of small PRs.
3. **Conventional Commits:** Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and PR titles (e.g., `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
4. **Testing:** Ensure changes are properly tested whenever possible (e.g., `go test -race ./...`).
5. **Compatibility Matrix:** Update `docs/opensubsonic_compatibility.md` whenever OpenSubsonic endpoints, stubs, or protocol features are added or changed.
