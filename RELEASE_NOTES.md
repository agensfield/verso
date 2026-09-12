# Verso v0.1.0-alpha.6

Early public alpha of the Codex account switcher.

This revision fixes switching after a historical Codex error and replaces permanent
progress lines with transient terminal spinners.

- Completed usage-limit errors no longer block switching after quota resets.
  Active turns still block, as do unknown or contradictory activity responses.
- Delayed, single-line progress covers account usage, runtime inspection, updates,
  switching, and device sign-in. It clears before prompts and results.
- JSON, redirected output, and dumb terminals remain free of animation; NO_COLOR
  disables spinner color.

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
