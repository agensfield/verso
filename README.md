# Verso

One Codex home. A few accounts. A deliberate switch.

Verso is a small Go CLI for switching Codex accounts while keeping native history,
memories, tools, skills, and plugins in place. **This is alpha software**, built
around the maintainer's daily workflow. macOS and Linux are supported; Codex
**0.152.0+** is the best-effort floor. The implementation currently tracks 0.154.0,
and compatibility can change with upstream.

## Install

```sh
brew install agensfield/tap/verso
```

Or download an archive for your OS/architecture from
[GitHub Releases](https://github.com/agensfield/verso/releases), verify it against
`checksums.txt`, and put `verso` on your `PATH`.

```sh
go install github.com/agensfield/verso/cmd/verso@latest
```

`verso update` updates a known release-binary or Go installation in place.
Homebrew installations use `brew upgrade verso`. Unknown local builds refuse
self-update. `verso update --check` checks without replacing the executable.

## Add accounts

Verso supports native ChatGPT credentials stored in a file. The local Codex
configuration should explicitly select:

```toml
cli_auth_credentials_store = "file"
```

Verso does not silently change this setting or migrate keyring/auto credentials.

```sh
verso import personal    # Save the current native Codex login
verso add work           # Device authorization directly into Verso's store
verso list               # Refresh accounts and show usage
verso list --cached      # Read saved information without refreshing
```

Aliases are optional and default to email. Email is not a unique identity across
workspaces; use an alias or the saved-account ID from `verso list --cached --json`
when lookup is ambiguous. Normal output focuses on account names, remaining usage,
reset times, and freshness.
Adding an account does not activate it. Expired inactive credentials refresh
silently when `list` fetches usage. Codex retains ownership of active-account refresh;
failed active usage reads stay unknown. There is no resident service or scheduler.

## Switch

```sh
verso preview work       # Check without changing accounts
verso switch work        # Review and confirm the switch
verso switch            # Account picker with quota and reset times
```

With a managed daemon, Verso checks visible turns, captures Herdr recovery metadata
when available, stops the daemon, saves the outgoing credentials, installs the
target, restarts, and verifies fresh-process/file/config evidence. Failed target
startup or verification attempts rollback, with its outcome reported separately.
Client reconnect failures do not undo a successful switch.

Without a daemon, Verso updates the credential file and tells you to close and
reopen Codex. Explicit local file mode is sufficient in this alpha, with a warning
that account-managed configuration fetched on startup may override it. Known local
configuration conflicts still refuse. Open standalone TUIs are advisory, including
mixed setups; they may retain the old account until reopened.

- Busy visible turns block immediately, including approval/input waits.
- Unknown or unreachable daemon state refuses; it is not treated as absence.
- Background memory/Chronicle activity is not completely observable. Warnings
  do not claim idleness or guaranteed self-healing. Native shutdown can interrupt
  background work and can escalate after its timeout.
- Known exhausted quota blocks unless you pass `--allow-exhausted`. Unknown quota
  warns and allows proceeding. There is no automatic fallback to another account.
- Herdr capture failure blocks unless you pass `--allow-no-snapshot`. No Herdr
  means no snapshot. One private checkpoint replaces the previous successful one.
- Switching asks for confirmation in your terminal. There is no force-cancel,
  delayed switch, or automatic restoration.

Visible-idle checks and selection rereads are snapshots. Verso's lock serializes
Verso processes, not Codex or other credential writers. Do not concurrently change
accounts through another tool. Fresh-process verification does not claim that
Codex exposes the full account identity over RPC.

## Inspect and recover

```sh
verso status
verso recovery --json
verso remove old-account
```

Recovery reports an unfinished transaction and the latest Herdr metadata. It does
not replay a switch or reconstruct panes. Inspect the current account/runtime
before manual recovery, and check for existing clients before recreating them.
Removing the selected account is refused; inactive removal deletes only Verso's copy.

Private state defaults to `~/.local/share/verso` (`VERSO_HOME` or `--state-dir`).
Native home follows `CODEX_HOME`, or `~/.codex` (`--codex-home`). `--codex-bin`
selects the native executable. Files are private and replaced atomically; credentials
are not encrypted separately. Protect this directory as you would your Codex login.
`--json` is available for structured results. `list` refreshes by default; use
`list --cached` for a read-only view. Login and switching prompt in your terminal.

Selected profiles, project overrides, and managed configurations outside the
conservatively supported inspection path can cause refusal. Custom saved provider
behavior remains Codex's responsibility; Verso does not convert threads to ChatGPT.
No legacy `codex-auth` migration, cross-machine sync, or per-thread account binding
is included.

## Agent-assisted use

Run `verso --skill` to read the guide bundled with your installed version.
It covers command permissions, JSON workflows, credential handling, and recovery.
The guide is available offline without configuring an account.

## Development

```sh
go test -race ./...
go vet ./...
go build ./cmd/verso
```

Tests use temporary homes, synthetic credentials, fake HTTP and runtime adapters.
Never test by restarting the daemon serving an agent's own active turn. Release
archives cover Darwin/Linux on amd64/arm64, with checksums and deterministic builds.
Linux fixture tests and cross-builds do not substitute for controlled human testing
on real accounts. Alpha readiness is refined through actual daily use.

MIT licensed. `verso licenses` prints the license and bundled dependency notices.
