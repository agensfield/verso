# Verso

A small account switcher for Codex. **Alpha, under development.**

Verso keeps one native Codex working environment and changes its selected account.
It supports local macOS/Linux, central app-server and standalone TUI setups.
Codex 0.152.0+ is the best-effort alpha floor; compatibility follows the version
used by the maintainer and may change with upstream.

The first alpha is being built. No release is ready yet.

## Contract

- Direct device login and current-login import; explicit file-backed credentials.
- Human-executed switches. Agents prepare previews, never restart their own runtime.
- Busy central turns block; unknown daemon state refuses. Background work and
  standalone TUIs warn. Existing standalone clients may keep old auth until reopened.
- No automatic account fallback or per-thread affinity.
- Target startup/identity failure attempts rollback; client reconnect failure does not.
- One private Herdr snapshot, no automatic restoration.
- No resident service, scheduler, inference warm-ups or legacy account migration.

## Development

```sh
go test -race ./...
go vet ./...
```

Coding worktrees live under gitignored `.worktrees/`. Do not test using a live
Codex home or daemon; use isolated fixtures. Public release artifacts and install
instructions will be added when the first alpha passes its gates.
