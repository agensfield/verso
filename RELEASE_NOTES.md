# Verso v0.1.0-alpha.4

Early public alpha of the Codex account switcher.

This revision makes `list` the account-and-usage view: it refreshes usage and
inactive credentials when needed, displays compact remaining-usage bars and reset
times, and keeps IDs in JSON. `list --cached` provides a read-only view. The separate
`quota` command is removed.

Normal help is short, with details under `verso help <command>`. Agent instructions
ship inside the binary at `verso --skill` (also `verso skill`). Switch safeguards
and Codex ownership of active-account refresh are unchanged.

- Direct device authorization and current-login import, with optional aliases.
- One native Codex home, private saved accounts, demand-driven quota and inactive refresh.
- Human-approved switching for managed-daemon, standalone, and mixed setups.
- Busy/unknown-state refusal, explicit quota/snapshot overrides, rollback and an unfinished-operation journal.
- One private Herdr recovery snapshot, with readout and no automatic restoration.
- macOS/Linux amd64/arm64 binaries, Go install, Homebrew, and install-aware updates.

Codex 0.152.0+ is a best-effort alpha floor; source behavior currently tracks 0.154.0.
Standalone success means the credential file was updated: reopen Codex to apply it,
and account-managed config may override local mode. Running-daemon verification is
fresh-process/file/config evidence, not a full server-reported identity API.

Validation includes synthetic race tests, vet, deterministic four-target archives,
independent bounded reviews, and a real-binary standalone PTY fixture. No live
account switch or shared-daemon restart was performed by the agents. Controlled
human use is the next acceptance step. Read the README's limits before switching.
