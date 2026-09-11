# Verso

Small Go CLI for human-operated Codex account switching. Keep the alpha simple.

- Do not use em dashes in code, comments, CLI output, or documentation.

- Read existing code and the task contract before editing. Use Go builtins first.
- Never read/log real credentials or switch/restart the shared Codex runtime for tests.
  Tests use temporary homes, synthetic credentials and fake process/RPC adapters.
- Agents may preview; only the human executes production account switches.
- Work in assigned repo-local `.worktrees/` with explicit file ownership. Do not
  change another lane's files or dependencies without coordinating with verso.
- Commit scoped changes conventionally. Do not push unless assigned release work.
- Run `go test -race ./...` and `go vet ./...`; use focused checks during iteration.
- Research/code claims are not live two-account acceptance receipts.
- Before retiring a merged worktree, its agent must stop editing, leave that cwd,
  and acknowledge completion. Never remove a running agent's cwd silently.
