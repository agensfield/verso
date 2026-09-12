// Package cli provides the operator and machine-readable CLI surfaces.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/buildinfo"
	"github.com/agensfield/verso/internal/codex"
	"github.com/agensfield/verso/internal/herdr"
	"github.com/agensfield/verso/internal/operation"
	"github.com/agensfield/verso/internal/quota"
	"github.com/agensfield/verso/internal/switcher"
	"github.com/agensfield/verso/internal/updater"
)

type App struct {
	UpdateAction       func(context.Context, bool) (updater.Result, error)
	StateDir           string
	CodexHome          string
	Binary             string
	Version            string
	Env                []string
	In                 io.Reader
	Out                io.Writer
	Err                io.Writer
	RunCommand         codex.CommandRunner
	CWD                string
	CredentialResolver codex.CredentialResolver
	Auth               DeviceAuthenticator
	json               bool
}

type response struct {
	Active      string                 `json:"active_account_id,omitempty"`
	Cached      bool                   `json:"cached,omitempty"`
	Update      *updater.Result        `json:"update,omitempty"`
	UpdateInfo  *updateMetadata        `json:"update_info,omitempty"`
	Plan        *switcher.Plan         `json:"plan,omitempty"`
	Quotas      map[string]quota.Entry `json:"quotas,omitempty"`
	QuotaInfo   map[string]quotaWire   `json:"quota_observations,omitempty"`
	Switch      *switcher.Result       `json:"switch_result,omitempty"`
	Journal     *switcher.Checkpoint   `json:"unfinished_switch,omitempty"`
	Snapshot    *herdr.Snapshot        `json:"herdr_snapshot,omitempty"`
	Schema      string                 `json:"schema"`
	OK          bool                   `json:"ok"`
	Command     string                 `json:"command"`
	Message     string                 `json:"message,omitempty"`
	Error       string                 `json:"error,omitempty"`
	ErrorCode   string                 `json:"error_code,omitempty"`
	Hint        string                 `json:"hint,omitempty"`
	Accounts    []accounts.Account     `json:"accounts"`
	Runtime     *codex.Observation     `json:"runtime,omitempty"`
	Target      *accounts.Account      `json:"target,omitempty"`
	VersionInfo *versionMetadata       `json:"version_info,omitempty"`
	Contract    *contractMetadata      `json:"contract,omitempty"`
	Selection   *selectionMetadata     `json:"selection,omitempty"`
	QuotaState  *quotaMetadata         `json:"quota_observation,omitempty"`
	Inventory   *inventoryMetadata     `json:"account_inventory,omitempty"`
}

type inventoryMetadata struct {
	Complete bool                    `json:"complete"`
	Issues   []accounts.AccountIssue `json:"issues"`
	Error    string                  `json:"error,omitempty"`
}

type versionMetadata struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	InstallKind string `json:"install_kind"`
}

type updateMetadata struct {
	Current     string         `json:"current"`
	Latest      string         `json:"latest"`
	Status      updater.Status `json:"status"`
	InstallKind string         `json:"install_kind"`
	Updated     bool           `json:"updated"`
	Guidance    string         `json:"guidance,omitempty"`
}

type selectionMetadata struct {
	Status string `json:"status"`
}

type quotaMetadata struct {
	Source    string `json:"source"`
	Complete  bool   `json:"complete"`
	Attempted int    `json:"attempted"`
	Available int    `json:"available"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
}

type quotaWire struct {
	Primary       *windowWire `json:"primary"`
	Secondary     *windowWire `json:"secondary"`
	Plan          *string     `json:"plan"`
	Exhausted     *bool       `json:"exhausted"`
	ObservedAt    *time.Time  `json:"observed_at"`
	CheckedAt     *time.Time  `json:"checked_at"`
	AttemptedAt   *time.Time  `json:"attempted_at"`
	Stale         bool        `json:"stale"`
	LoginRequired bool        `json:"login_required"`
	Warning       string      `json:"warning,omitempty"`
}

type windowWire struct {
	UsedPercent    *float64   `json:"used_percent"`
	WindowSeconds  *int64     `json:"window_seconds"`
	ResetInSeconds *int64     `json:"reset_in_seconds"`
	ResetsAt       *time.Time `json:"resets_at"`
}

type commandMetadata struct {
	Name          string   `json:"name"`
	Flags         []string `json:"flags"`
	Effects       []string `json:"effects"`
	HumanRequired bool     `json:"human_required"`
}

type contractMetadata struct {
	ResponseSchema string            `json:"response_schema"`
	Commands       []commandMetadata `json:"commands"`
	Notes          []string          `json:"notes"`
}

const usage = `Verso: switch Codex accounts

Usage: verso <command>

  list [account]        Accounts and usage
  switch [account]      Switch accounts
  add [alias]           Add an account
  import [alias]        Save your current login
  alias <account> <new> Rename a saved account
  remove <account>      Remove a saved account
  preview <account>     Check before switching

More: status, recovery, update, version, licenses
Help: verso help <command|options>     Agent guide: verso --skill
`

func (a *App) Run(ctx context.Context, args []string) int {
	if a.In == nil {
		a.In = os.Stdin
	}
	if a.Out == nil {
		a.Out = os.Stdout
	}
	if a.Err == nil {
		a.Err = os.Stderr
	}
	fs := flag.NewFlagSet("verso", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&a.json, "json", false, "")
	fs.StringVar(&a.StateDir, "state-dir", a.StateDir, "")
	fs.StringVar(&a.CodexHome, "codex-home", a.CodexHome, "")
	fs.StringVar(&a.Binary, "codex-bin", a.Binary, "")
	skill := fs.Bool("skill", false, "")
	version := fs.Bool("version", false, "")
	shortVersion := fs.Bool("v", false, "")
	cached := fs.Bool("cached", false, "")
	checkUpdate := fs.Bool("check", false, "")
	allowExhausted := fs.Bool("allow-exhausted", false, "")
	allowNoSnapshot := fs.Bool("allow-no-snapshot", false, "")
	intent := scanInvocation(args)
	a.json = intent.json
	ordered, err := flagsFirst(fs, args)
	if err == nil {
		err = fs.Parse(ordered)
	}
	// Parsing may stop before a later --json value. The invocation scan is the
	// authority for output mode because it respects values and -- boundaries.
	a.json = intent.json
	if errors.Is(err, flag.ErrHelp) {
		return a.printHelp(helpTopic(ordered))
	}
	if err != nil {
		return a.finish(response{Command: "usage"}, errors.New("invalid arguments; see verso --help"))
	}
	if *skill {
		if len(fs.Args()) != 0 || flagsOutside(intent.flags, "json", "skill", "state-dir", "codex-home", "codex-bin") {
			return a.finish(response{Command: "skill"}, errors.New("use verso --skill without a command"))
		}
		r := response{Command: "skill", Message: agentGuide}
		if a.json {
			r.Contract = machineContract()
		}
		return a.finish(r, nil)
	}
	if *version || *shortVersion {
		if len(fs.Args()) != 0 || flagsOutside(intent.flags, "json", "version", "v", "state-dir", "codex-home", "codex-bin") {
			return a.finish(response{Command: "version"}, errors.New("use --version without a command"))
		}
		return a.finish(a.versionResponse(), nil)
	}
	pos := fs.Args()
	if len(pos) == 0 {
		if flagsOutside(intent.flags, "json", "state-dir", "codex-home", "codex-bin") {
			return a.finish(response{Command: "usage"}, errors.New("command-only flag requires a command; see verso --help"))
		}
		if a.json {
			return a.finish(response{Command: "help", Message: usage}, nil)
		}
		return writeExit(a.Out, usage)
	}
	command := pos[0]
	pos = pos[1:]
	if err := validateCommandFlags(command, intent.flags); err != nil {
		return a.finish(response{Command: command}, err)
	}
	if command == "help" {
		if len(pos) > 1 {
			return a.finish(response{Command: "help"}, errors.New("usage: verso help [command|options]"))
		}
		topic := ""
		if len(pos) == 1 {
			topic = pos[0]
		}
		return a.printHelp(topic)
	}
	if command == "skill" {
		if len(pos) != 0 || *cached || *checkUpdate || *allowExhausted || *allowNoSnapshot {
			return a.finish(response{Command: command}, errors.New("usage: verso skill"))
		}
		r := response{Command: command, Message: agentGuide}
		if a.json {
			r.Contract = machineContract()
		}
		return a.finish(r, nil)
	}
	if command == "schema" && len(pos) == 0 {
		message := contractText(machineContract())
		return a.finish(response{Command: command, Message: message, Contract: machineContract()}, nil)
	}
	if command == "licenses" && len(pos) == 0 {
		return a.finish(response{Command: command, Message: buildinfo.Licenses}, nil)
	}
	if command == "version" && len(pos) == 0 {
		return a.finish(a.versionResponse(), nil)
	}
	if command == "version" || command == "licenses" || command == "schema" {
		return a.finish(response{Command: command}, fmt.Errorf("usage: verso %s", command))
	}
	if command == "update" {
		return a.updateCommand(ctx, pos, *checkUpdate)
	}
	if command == "list" {
		return a.listCommand(ctx, pos, *cached)
	}
	if command == "switch" {
		return a.switchAccount(ctx, pos, *allowExhausted, *allowNoSnapshot)
	}
	if command == "import" || command == "remove" {
		return a.accountMutation(ctx, command, pos)
	}
	if command == "alias" {
		return a.aliasAccount(ctx, pos)
	}
	if command == "add" {
		return a.add(ctx, pos)
	}
	if command != "recovery" && command != "status" && command != "preview" {
		return a.finish(response{Command: command}, errors.New("unknown command; see verso --help"))
	}
	if (command == "preview" && len(pos) != 1) || (command != "preview" && len(pos) != 0) {
		return a.finish(response{Command: command}, errors.New("unexpected or missing command arguments"))
	}
	if !filepath.IsAbs(a.StateDir) || !filepath.IsAbs(a.CodexHome) {
		return a.finish(response{Command: command}, errors.New("state and Codex home paths must be absolute"))
	}

	if command == "recovery" {
		r := response{Command: command}
		var err error
		r.Journal, err = (operation.Journal{Root: a.StateDir}).Read()
		if err != nil {
			return a.finish(r, err)
		}
		r.Snapshot, err = herdr.Read(a.StateDir)
		if err != nil {
			return a.finish(r, err)
		}
		r.Message = "No unfinished switch recorded."
		if r.Journal != nil {
			r.Message = "Unfinished switch recorded. Inspect the selected account and runtime before any manual recovery; no restoration has been attempted."
		}
		if r.Snapshot != nil {
			r.Message += " A Herdr checkpoint is available; use --json to view its recovery metadata. Check current panes before recreating any clients."
		}
		if store, openErr := accounts.OpenReadOnly(filepath.Join(a.StateDir, "accounts")); openErr == nil {
			r.Accounts, _, _ = store.ListPartial()
		}
		return a.finish(r, nil)
	}
	store, storeErr := accounts.OpenReadOnly(filepath.Join(a.StateDir, "accounts"))
	saved := []accounts.Account{}
	issues := []accounts.AccountIssue{}
	if storeErr == nil {
		saved, issues, storeErr = store.ListPartial()
	}
	r := response{Command: command, Accounts: saved, Inventory: inventoryResult(issues, storeErr)}
	if command == "preview" {
		if storeErr != nil {
			return a.finish(r, storeErr)
		}
		target, e := findInspectionAccount(store, saved, issues, pos[0])
		if e != nil {
			return a.finish(r, e)
		}
		r.Target = &target
		backend, e := a.backend()
		if e != nil {
			return a.finish(r, e)
		}
		a.progress("Inspecting switch conditions...")
		plan, e := (switcher.Engine{Backend: backend}).Preview(ctx, switcher.Request{Target: target.ID, AllowExhausted: *allowExhausted, AllowNoSnapshot: *allowNoSnapshot})
		r.Plan = &plan
		r.Message = "Preview only; no credentials were refreshed or activated."
		return a.finish(r, e)
	}
	inspector, err := a.inspector()
	if err != nil {
		return a.finish(r, err)
	}
	a.progress("Inspecting Codex runtime...")
	o, err := inspector.Inspect(ctx)
	r.Runtime = &o
	if err != nil {
		return a.finish(r, err)
	}

	return a.finish(r, nil)
}

func inventoryResult(issues []accounts.AccountIssue, err error) *inventoryMetadata {
	result := &inventoryMetadata{Complete: err == nil && len(issues) == 0, Issues: issues}
	if result.Issues == nil {
		result.Issues = []accounts.AccountIssue{}
	}
	if err != nil {
		result.Error = "account inventory unavailable"
	}
	return result
}

func findInspectionAccount(store *accounts.Store, saved []accounts.Account, issues []accounts.AccountIssue, query string) (accounts.Account, error) {
	account, err := store.Find(query)
	if err == nil || len(issues) == 0 {
		return account, err
	}
	matches := make([]accounts.Account, 0, 1)
	for _, candidate := range saved {
		if candidate.ID == query || candidate.Alias == query || candidate.Email == query {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return accounts.Account{}, err
	case 1:
		return matches[0], nil
	default:
		return accounts.Account{}, accounts.ErrAmbiguous
	}
}

func (a *App) versionResponse() response {
	return response{Command: "version", Message: a.Version, VersionInfo: &versionMetadata{Version: a.Version, Commit: buildinfo.Commit(), InstallKind: buildinfo.InstallKind()}}
}

func machineContract() *contractMetadata {
	return &contractMetadata{
		ResponseSchema: "verso/v1",
		Commands: []commandMetadata{
			{Name: "list", Flags: []string{"--cached", "--json"}, Effects: []string{"account-read", "quota-network", "credential-refresh", "quota-cache-write"}},
			{Name: "list --cached", Flags: []string{"--json"}, Effects: []string{"account-read", "quota-cache-read"}},
			{Name: "status", Flags: []string{"--json"}, Effects: []string{"account-read", "runtime-inspection"}},
			{Name: "preview", Flags: []string{"--allow-exhausted", "--allow-no-snapshot", "--json"}, Effects: []string{"account-read", "runtime-inspection", "quota-cache-read"}},
			{Name: "recovery", Flags: []string{"--json"}, Effects: []string{"journal-read", "snapshot-read"}},
			{Name: "import", Flags: []string{"--json"}, Effects: []string{"native-selection-read", "account-write"}},
			{Name: "alias", Flags: []string{"--json"}, Effects: []string{"account-write"}},
			{Name: "remove", Flags: []string{"--json"}, Effects: []string{"native-selection-read", "account-delete"}},
			{Name: "add", Flags: []string{}, Effects: []string{"device-authorization", "account-write"}, HumanRequired: true},
			{Name: "switch", Flags: []string{"--allow-exhausted", "--allow-no-snapshot"}, Effects: []string{"credential-activation", "daemon-restart", "journal-write", "snapshot-write"}, HumanRequired: true},
			{Name: "update", Flags: []string{"--check", "--json"}, Effects: []string{"release-network", "binary-write"}},
			{Name: "schema", Flags: []string{"--json"}, Effects: []string{}},
		},
		Notes: []string{"missing observations are unknown, not false", "ok can be true when quota refresh is partial", "JSON stdout is one final document"},
	}
}

func contractText(contract *contractMetadata) string {
	var b strings.Builder
	b.WriteString("verso/v1 commands and effects\n")
	for _, command := range contract.Commands {
		effects := "none"
		if len(command.Effects) > 0 {
			effects = strings.Join(command.Effects, ", ")
		}
		fmt.Fprintf(&b, "  %-15s %s\n", command.Name, effects)
	}
	b.WriteString("Use verso schema --json for flags and machine-readable metadata.")
	return b.String()
}

func (a *App) finish(r response, err error) int {
	err = publicError(err)
	if r.Accounts == nil {
		r.Accounts = []accounts.Account{}
	}
	r.Schema = "verso/v1"
	r.OK = err == nil
	if err != nil {
		r.Error = err.Error()
		r.ErrorCode, r.Hint = classifyError(r.Command, err)
	}
	if a.json {
		e := json.NewEncoder(a.Out)
		e.SetIndent("", "  ")
		if e.Encode(r) != nil {
			return 1
		}
	} else {
		failedWrite := false
		writef := func(w io.Writer, format string, args ...any) {
			if failedWrite {
				return
			}
			_, writeErr := fmt.Fprintf(w, format, args...)
			failedWrite = writeErr != nil
		}
		if r.Message != "" {
			writef(a.Out, "%s\n", r.Message)
		}
		if r.Command == "list" && len(r.Accounts) > 0 {
			if renderErr := a.renderAccounts(r); renderErr != nil {
				_, _ = fmt.Fprintln(a.Err, "verso:", renderErr)
				return 1
			}
		}
		if r.Journal != nil {
			writef(a.Out, "Recorded phase: %s\nPrevious account: %s\nRequested account: %s\nNext: run verso status, then inspect verso recovery --json before retrying.\n", recoveryPhase(r.Journal.Phase), checkpointAccountName(r.Accounts, r.Journal.From), checkpointAccountName(r.Accounts, r.Journal.Target))
		}
		if r.Switch != nil && r.Switch.RollbackAttempted {
			if r.Switch.RollbackSucceeded {
				writef(a.Out, "Rollback: previous account restored and verified.\n")
			} else {
				writef(a.Out, "Rollback: incomplete; inspect verso recovery before further changes.\n")
			}
		}
		if r.Switch != nil {
			for _, warning := range r.Switch.Warnings {
				writef(a.Out, "%s %s\n", a.humanHeading("Warning:"), warning)
			}
		}
		if r.Plan != nil {
			writef(a.Out, "%s %s\n", a.humanHeading("Codex:"), r.Plan.Daemon)
			if !r.Plan.UnfinishedKnown {
				writef(a.Out, "%s unavailable\n", a.humanHeading("Recovery journal:"))
			} else if r.Plan.Unfinished {
				writef(a.Out, "%s unfinished switch recorded\n", a.humanHeading("Recovery journal:"))
			}
			if len(r.Plan.Busy) > 0 {
				writef(a.Out, "%s %d\n", a.humanHeading("Busy conversations:"), len(r.Plan.Busy))
			}
			for _, warning := range r.Plan.Warnings {
				writef(a.Out, "%s %s\n", a.humanHeading("Warning:"), warning)
			}
		}
		if r.Runtime != nil {
			writef(a.Out, "%s %s\n%s %s\n%s %s\n", a.humanHeading("Codex:"), r.Runtime.Daemon, a.humanHeading("Credentials:"), r.Runtime.Config.CredentialStore, a.humanHeading("Selected login:"), selectedAccountName(r))
			writef(a.Out, "%s %s\n", a.humanHeading("Credential proof:"), credentialProofLabel(r.Runtime.Credential))
			if r.Runtime.Credential.Reason != "" {
				writef(a.Out, "%s %s\n", a.humanHeading("Credential detail:"), r.Runtime.Credential.Reason)
			}
			if r.Runtime.Credential.Warning != "" {
				writef(a.Out, "%s %s\n", a.humanHeading("Warning:"), r.Runtime.Credential.Warning)
			}
			if r.Runtime.ActivityKnown {
				writef(a.Out, "%s %s\n", a.humanHeading("Activity:"), activityLabel(len(r.Runtime.Busy)))
			} else {
				writef(a.Out, "%s unavailable\n", a.humanHeading("Activity:"))
				if r.Runtime.ActivityError != "" {
					writef(a.Out, "%s %s\n", a.humanHeading("Activity detail:"), r.Runtime.ActivityError)
				}
			}
			if summary := clientSummary(r.Runtime.Clients); summary != "" {
				writef(a.Out, "%s %s\n", a.humanHeading("Process candidates:"), summary)
			}
			for _, warning := range r.Runtime.Warnings {
				writef(a.Out, "%s %s\n", a.humanHeading("Warning:"), warning)
			}
		}
		if r.Inventory != nil && !r.Inventory.Complete {
			if r.Inventory.Error != "" {
				writef(a.Out, "%s %s\n", a.humanHeading("Account inventory:"), r.Inventory.Error)
			} else {
				writef(a.Out, "%s %d saved account issue(s); healthy entries shown only\n", a.humanHeading("Account inventory:"), len(r.Inventory.Issues))
			}
		}
		if r.Target != nil {
			writef(a.Out, "%s %s\n", a.humanHeading("Account:"), accountChoiceName(*r.Target))
		}
		if err != nil {
			writef(a.Err, "verso: %s\n", r.Error)
			if r.Hint != "" {
				writef(a.Err, "hint: %s\n", r.Hint)
			}
		}
		if failedWrite {
			return 1
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

func publicError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{
		accounts.ErrNotFound, accounts.ErrAmbiguous, accounts.ErrUnsafePath,
		accounts.ErrUnknownActive, accounts.ErrActiveAccount, accounts.ErrIdentityMismatch,
		accounts.ErrInvalidSchema, accounts.ErrInvalidAlias, accounts.ErrAliasConflict,
		accounts.ErrReadOnly,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return errors.New("filesystem operation failed")
	}
	return err
}

type invocationIntent struct {
	json  bool
	flags map[string]bool
}

func scanInvocation(args []string) invocationIntent {
	result := invocationIntent{flags: make(map[string]bool)}
	valueFlags := map[string]bool{"state-dir": true, "codex-home": true, "codex-bin": true}
	for n := 0; n < len(args); n++ {
		arg := args[n]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		name, value, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "h" {
			name = "help"
		}
		result.flags[name] = true
		if name == "json" {
			result.json = !assigned
			if assigned {
				parsed, parseErr := strconv.ParseBool(value)
				result.json = parseErr == nil && parsed
			}
		}
		if valueFlags[name] && !assigned {
			n++
		}
	}
	return result
}

func validateCommandFlags(command string, seen map[string]bool) error {
	if !knownCommand(command) {
		return nil
	}
	global := map[string]bool{"json": true, "state-dir": true, "codex-home": true, "codex-bin": true, "help": true}
	allowed := map[string]map[string]bool{
		"list": {"cached": true}, "switch": {"allow-exhausted": true, "allow-no-snapshot": true},
		"preview": {"allow-exhausted": true, "allow-no-snapshot": true}, "update": {"check": true},
	}
	for name := range seen {
		if global[name] || allowed[command][name] {
			continue
		}
		return fmt.Errorf("flag --%s is not valid for %s", name, command)
	}
	return nil
}

func knownCommand(command string) bool {
	switch command {
	case "help", "skill", "schema", "licenses", "version", "update", "list", "switch", "import", "remove", "add", "alias", "recovery", "status", "preview":
		return true
	default:
		return false
	}
}

func flagsOutside(seen map[string]bool, allowed ...string) bool {
	wanted := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		wanted[name] = true
	}
	for name := range seen {
		if !wanted[name] {
			return true
		}
	}
	return false
}

func classifyError(command string, err error) (string, string) {
	message := err.Error()
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled", "retry when ready"
	case errors.Is(err, accounts.ErrNotFound):
		return "account_not_found", "run verso list --cached"
	case errors.Is(err, accounts.ErrAmbiguous):
		return "account_ambiguous", "use an alias or ID from verso list --cached --json"
	case errors.Is(err, accounts.ErrInvalidAlias), errors.Is(err, accounts.ErrAliasConflict):
		return "invalid_alias", "choose a distinct account alias"
	case errors.Is(err, accounts.ErrUnsafePath), errors.Is(err, accounts.ErrActiveAccount), errors.Is(err, accounts.ErrUnknownActive):
		return "safety_refusal", "inspect with verso status --json before retrying"
	case errors.Is(err, switcher.ErrUnfinished):
		return "recovery_required", "run verso recovery --json"
	case errors.Is(err, switcher.ErrUnknown):
		return "inspection_unavailable", "run verso status --json"
	case errors.Is(err, switcher.ErrBusy), errors.Is(err, switcher.ErrBackend), errors.Is(err, switcher.ErrChanged), errors.Is(err, switcher.ErrExhausted):
		return "safety_refusal", "review verso preview --json before retrying"
	case strings.Contains(message, "usage:") || strings.Contains(message, "invalid arguments") || strings.Contains(message, "not valid for") || strings.Contains(message, "unexpected or missing") || strings.Contains(message, "command-only flag") || strings.HasPrefix(message, "use --") || strings.HasPrefix(message, "use verso --"):
		if _, ok := commandHelp[command]; ok && command != "options" {
			return "invalid_arguments", "run verso help " + command
		}
		return "invalid_arguments", "run verso --help"
	case strings.Contains(message, "unfinished") || strings.Contains(message, "recovery"):
		return "recovery_required", "run verso recovery --json"
	case strings.Contains(message, "unknown command"):
		if command == "quota" {
			return "unknown_command", "use verso list"
		}
		return "unknown_command", "run verso --help"
	default:
		return "operation_failed", ""
	}
}

func writeExit(out io.Writer, value string) int {
	if _, err := io.WriteString(out, value); err != nil {
		return 1
	}
	return 0
}

func credentialProofLabel(proof codex.CredentialProof) string {
	switch proof.Status {
	case codex.CredentialLocalFile:
		return "effective local file mode resolved"
	case codex.CredentialFileSelected:
		return "selected login matches effective file mode"
	case codex.CredentialFreshProcess:
		return "selected login verified in a fresh Codex process"
	case codex.CredentialUnknown, "":
		return "unverified"
	default:
		return "unverified (see JSON for status)"
	}
}

func activityLabel(busy int) string {
	if busy == 0 {
		return "no busy conversations observed"
	}
	if busy == 1 {
		return "1 busy conversation observed"
	}
	return fmt.Sprintf("%d busy conversations observed", busy)
}

func clientSummary(clients []codex.Client) string {
	counts := map[codex.ClientKind]int{}
	for _, client := range clients {
		counts[client.Kind]++
	}
	parts := make([]string, 0, 3)
	for _, item := range []struct {
		kind  codex.ClientKind
		label string
	}{{codex.ClientAttached, "attached intent"}, {codex.ClientStandalone, "standalone runtime"}, {codex.ClientUnknown, "unknown role"}} {
		if count := counts[item.kind]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, item.label))
		}
	}
	return strings.Join(parts, ", ")
}

func recoveryPhase(phase string) string {
	switch phase {
	case "prepared":
		return "prepared, no runtime change recorded"
	case "stopping":
		return "stopping the previous runtime"
	case "activating":
		return "saving the requested credentials"
	case "starting":
		return "starting the requested runtime"
	case "committed":
		return "verification completed, cleanup incomplete"
	case "rolling_back":
		return "restoring the previous account"
	case "rolled_back":
		return "previous account restored, cleanup incomplete"
	default:
		return "unknown"
	}
}

func checkpointAccountName(saved []accounts.Account, id string) string {
	if id == "" {
		return "none recorded"
	}
	for _, account := range saved {
		if account.ID == id {
			return accountChoiceName(account)
		}
	}
	return "unresolved saved account"
}

// flagsFirst permits familiar `verso list --json` without a custom flag parser.
// The standard flag package still owns validation, value parsing and errors.
func flagsFirst(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, pos []string
	for n := 0; n < len(args); n++ {
		arg := args[n]
		if arg == "--" {
			pos = append(pos, args[n+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			pos = append(pos, arg)
			continue
		}
		name, _, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "help" || name == "h" {
			flags = append(flags, arg)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			return nil, errors.New("unknown flag")
		}
		flags = append(flags, arg)
		isBool, ok := f.Value.(interface{ IsBoolFlag() bool })
		if !assigned && (!ok || !isBool.IsBoolFlag()) {
			n++
			if n >= len(args) {
				return nil, errors.New("flag needs a value")
			}
			flags = append(flags, args[n])
		}
	}
	return append(flags, append([]string{"--"}, pos...)...), nil
}

// DeviceAuthenticator separates network enrollment from CLI persistence.
type DeviceAuthenticator interface {
	DeviceLogin(context.Context, func(auth.DevicePrompt) error) ([]byte, error)
}
