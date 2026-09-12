# Verso v0.1.0-alpha.5

Early public alpha of the Codex account switcher.

This revision completes the human and agent CLI flows and fixes misleading runtime
status. Centrally attached Codex sessions started without an explicit `--remote`
argument are no longer called standalone. Verified daemon connectivity survives an
incomplete conversation check; switching still refuses when activity is unknown.

- Reject misplaced command flags before mutations and preserve JSON on errors/help.
- Reliable cancellation, honest partial quota, and safe recovery-journal inspection.
- Editable aliases, clearer account choices, width-aware output, and useful warnings.
- Visible progress and informative status, preview, recovery, and mutation receipts.
- Offline machine discovery and additive structured observation/error metadata.

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
