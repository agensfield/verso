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
	Active    string                 `json:"active_account_id,omitempty"`
	Cached    bool                   `json:"cached,omitempty"`
	Update    *updater.Result        `json:"update,omitempty"`
	Plan      *switcher.Plan         `json:"plan,omitempty"`
	Quotas    map[string]quota.Entry `json:"quotas,omitempty"`
	Switch    *switcher.Result       `json:"switch_result,omitempty"`
	Journal   *switcher.Checkpoint   `json:"unfinished_switch,omitempty"`
	Snapshot  *herdr.Snapshot        `json:"herdr_snapshot,omitempty"`
	Schema    string                 `json:"schema"`
	OK        bool                   `json:"ok"`
	Command   string                 `json:"command"`
	Message   string                 `json:"message,omitempty"`
	Error     string                 `json:"error,omitempty"`
	ErrorCode string                 `json:"error_code,omitempty"`
	Hint      string                 `json:"hint,omitempty"`
	Accounts  []accounts.Account     `json:"accounts,omitempty"`
	Runtime   *codex.Observation     `json:"runtime,omitempty"`
	Target    *accounts.Account      `json:"target,omitempty"`
}

const usage = `Verso: switch Codex accounts

Usage: verso <command>

  list [account]        Accounts and usage
  switch [account]      Switch accounts
  add [alias]           Add an account
  import [alias]        Save your current login
  remove <account>      Remove a saved account
  preview <account>     Check before switching

More: status, recovery, update, version, licenses
Help: verso help <command>     Agent guide: verso --skill
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
	if errors.Is(err, flag.ErrHelp) {
		return a.printHelp(helpTopic(ordered))
	}
	if err != nil {
		return a.finish(response{Command: "usage"}, errors.New("invalid arguments; see verso --help"))
	}
	if *skill {
		if len(fs.Args()) != 0 || *version || *shortVersion || *cached || *checkUpdate || *allowExhausted || *allowNoSnapshot {
			return a.finish(response{Command: "skill"}, errors.New("use verso --skill without a command"))
		}
		return a.finish(response{Command: "skill", Message: agentGuide}, nil)
	}
	if *version || *shortVersion {
		if len(fs.Args()) != 0 {
			return a.finish(response{Command: "version"}, errors.New("use --version without a command"))
		}
		return a.finish(response{Command: "version", Message: a.Version}, nil)
	}
	pos := fs.Args()
	if len(pos) == 0 {
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
		return a.finish(response{Command: command, Message: agentGuide}, nil)
	}
	if command == "licenses" && len(pos) == 0 {
		return a.finish(response{Command: command, Message: buildinfo.Licenses}, nil)
	}
	if command == "version" && len(pos) == 0 {
		return a.finish(response{Command: command, Message: a.Version}, nil)
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
		return a.finish(r, nil)
	}
	store, err := accounts.OpenReadOnly(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		return a.finish(response{Command: command}, err)
	}
	saved, err := store.List()
	if err != nil {
		return a.finish(response{Command: command}, err)
	}
	r := response{Command: command, Accounts: saved}
	if command == "preview" {
		target, e := store.Find(pos[0])
		if e != nil {
			return a.finish(r, e)
		}
		r.Target = &target
		backend, e := a.backend()
		if e != nil {
			return a.finish(r, e)
		}
		plan, e := (switcher.Engine{Backend: backend}).Preview(ctx, switcher.Request{Target: target.ID, AllowExhausted: *allowExhausted, AllowNoSnapshot: *allowNoSnapshot})
		r.Plan = &plan
		r.Message = "Preview only; no credentials were refreshed or activated."
		return a.finish(r, e)
	}
	inspector, err := a.inspector()
	if err != nil {
		return a.finish(r, err)
	}
	o, err := inspector.Inspect(ctx)
	r.Runtime = &o
	if err != nil {
		return a.finish(r, err)
	}

	return a.finish(r, nil)
}

func (a *App) finish(r response, err error) int {
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
			writef(a.Out, "Phase: %s\nPrevious: %q\nRequested: %q\n", r.Journal.Phase, r.Journal.From, r.Journal.Target)
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
			if len(r.Runtime.Busy) > 0 {
				writef(a.Out, "%s %d\n", a.humanHeading("Busy conversations:"), len(r.Runtime.Busy))
			}
			for _, warning := range r.Runtime.Warnings {
				writef(a.Out, "%s %s\n", a.humanHeading("Warning:"), warning)
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

func classifyError(command string, err error) (string, string) {
	message := err.Error()
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled", "retry when ready"
	case strings.Contains(message, "usage:") || strings.Contains(message, "invalid arguments") || strings.Contains(message, "not valid for"):
		return "invalid_arguments", "run verso help " + command
	case strings.Contains(message, "not found"):
		return "account_not_found", "run verso list --cached"
	case strings.Contains(message, "ambiguous"):
		return "account_ambiguous", "use an alias or ID from verso list --cached --json"
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
	if proof.Status == "" {
		return "unknown"
	}
	return string(proof.Status)
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
