# Verso agent guide

These instructions are bundled with the installed binary. Use `verso version`
to identify the build and `verso --help` for its command list. The guide is also
available as `verso skill`; add `--json` to receive it in the normal JSON envelope.
Reading this guide requires no account setup, network connection, or running Codex.

## What Verso manages

Verso saves Codex accounts and changes the selected credentials in one real Codex
home. History, memories, tools, skills, and plugins stay there. Email is not a
unique account identity across workspaces. Resolve ambiguous names using the
saved account ID from `verso list --cached --json`; do not silently choose a
matching email.

## Start with inspection

```sh
verso version
verso list --cached --json
verso status --json
verso preview work --json
```

`list --cached`, `status`, `preview`, and `recovery` do not enroll accounts,
refresh credentials, activate an account, or restart a daemon. `list --cached`
reads saved account metadata and quota without network, RPC, or state writes.
`preview` uses cached quota; missing or stale quota is unknown, not zero usage or
proof of exhaustion. Status and switch previews inspect conversation activity and
may report unhealthy threads. Current-login import does not require conversations
to be idle or healthy.

JSON responses use `schema: "verso/v1"`, an `ok` flag, and an `error` on failure.
Failures also include a stable `error_code` and may include a safe `hint`. Check both
the exit status and `ok`. Do not treat missing fields as confirmed state. Run
`verso schema --json` for offline command, flag, and effect metadata. `--json`
applies to the whole invocation, including argument failures and help.

List results separate account inventory, selected-login observation, and quota
observation. `accounts` is an empty array when none are saved. `selection.status`
states whether selection was verified, unmatched, absent, unknown, or not inspected.
`quota_observation.complete` can be false while `ok` remains true because saved
account discovery succeeded independently of one or more quota requests.

## Save or inspect accounts when requested

```sh
verso import personal --json
verso alias personal home --json
verso list personal --json
verso list --cached --json
verso remove old-account --json
```

- `import` saves the current native login without activating another account.
- `alias <account> <alias>` changes display metadata only. Use it to resolve
  ambiguous emails; it does not alter native account identity or credentials.
- `add [alias]` starts device authorization directly into Verso's store. It needs
  human participation and does not support `--json`. Start it only when requested;
  keep device codes out of logs, shared notes, and source files.
- `list` refreshes account metadata and quota cache. It may rotate inactive saved
  credentials when needed. It never independently refreshes active credentials;
  Codex owns that refresh, and a failed active quota read does not authorize Verso
  to take it over. Use `list --cached --json` when the operation must be read-only.
- `remove` deletes only Verso's saved copy and refuses the selected account.

Account changes need the user's authorization. Prefer Verso's commands over
reading or editing credential files directly. Never print tokens, auth documents,
or credential-store contents. The JSON account list exposes metadata only.

## Prepare a switch for the operator

Run `verso preview <account> --json`, explain any blockers or warnings, then give
the operator the command to run in their terminal:

```sh
verso switch work
```

Agents must not execute account switches or restart the daemon serving their turn.
There is no agent `--yes` bypass. Do not strip agent environment variables, create
a pseudo-terminal, or invoke native daemon commands to evade this boundary.
Synthetic test fixtures with fake runtimes are separate from operating real accounts.

Do not add `--allow-exhausted` or `--allow-no-snapshot` automatically. The operator
must choose those tradeoffs. No automatic account fallback or queued switch exists.

Common safe failure handling:

- `account_not_found`: refresh the saved inventory with `list --cached --json`.
- `account_ambiguous`: use a distinct alias or exact saved ID, never guess.
- `cancelled`: no approval is implied; let the operator retry when ready.
- `recovery_required`: inspect `recovery --json` before another mutation.
- `invalid_arguments`: follow the returned hint or command help.

With a managed daemon, visible active turns block switching, including approval
and input waits. Unknown or unreachable daemon state also blocks. Do not force
cancellation or call a failed probe proof of absence. Background work is not fully
observable, and visible-idle checks do not freeze new work or guarantee safe drain.

Without a daemon, explicit local file mode can authorize a credential-file update
with a managed-configuration warning. Tell the operator to reopen Codex; do not
claim that local inspection proves future cloud configuration. In mixed setups,
standalone clients may retain old credentials after the managed daemon restarts.
There is no per-thread account binding, and saved custom-provider behavior remains
Codex's responsibility.

## Handle failures and recovery

```sh
verso recovery --json
```

An unfinished switch needs inspection, not blind retry. Do not delete journals
or lock files to make a command proceed. Report the original switch failure and
rollback outcome separately. Successful verification does not guarantee every TUI
reconnected; a client reconnect failure is not a reason to switch back.

Herdr recovery is one private metadata snapshot, replaced after successful capture.
It contains layout and agent/thread locators, not terminal transcripts. No Herdr
means no snapshot. There is no automatic restoration; inspect current panes before
suggesting recovery so existing clients are not duplicated.

## Configuration and updates

Verso data defaults to `~/.local/share/verso`, selected by `VERSO_HOME` or
`--state-dir`. The native home follows `CODEX_HOME`, then `~/.codex`; use
`--codex-home` and `--codex-bin` when explicitly targeting another installation.
Do not guess a different home to get around an inspection failure.

Native credentials must use `cli_auth_credentials_store = "file"`. Do not silently
change Codex configuration, copy the whole Codex home, or introduce a shadow home.
Verso stores private files without a separate encryption or unlock layer; protect
them like the native login.

`verso update --check` checks public releases. `verso update` replaces a known
binary/Go installation; Homebrew installations use `brew upgrade verso`. Request
or retain authorization before updating. Never overwrite Homebrew-managed files
manually or infer update provenance for an unknown build.
