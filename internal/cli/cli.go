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
	"strings"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
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
	Update   *updater.Result        `json:"update,omitempty"`
	Plan     *switcher.Plan         `json:"plan,omitempty"`
	Quotas   map[string]quota.Entry `json:"quotas,omitempty"`
	Switch   *switcher.Result       `json:"switch_result,omitempty"`
	Journal  *switcher.Checkpoint   `json:"unfinished_switch,omitempty"`
	Snapshot *herdr.Snapshot        `json:"herdr_snapshot,omitempty"`
	Schema   string                 `json:"schema"`
	OK       bool                   `json:"ok"`
	Command  string                 `json:"command"`
	Message  string                 `json:"message,omitempty"`
	Error    string                 `json:"error,omitempty"`
	Accounts []accounts.Account     `json:"accounts,omitempty"`
	Runtime  *codex.Observation     `json:"runtime,omitempty"`
	Target   *accounts.Account      `json:"target,omitempty"`
}

const usage = `Verso — Codex account switching (alpha, under development)

Usage: verso [options] <command>

  switch [account]      Select an account with human approval
  import [alias]        Save the current native Codex login
  remove <account>      Delete an inactive saved account
  add [alias]           Save an account using device authorization
  quota [account]       Fetch quota on demand; --refresh bypasses recent cache
  list                  List saved accounts without refreshing credentials
  status                Inspect local native account/runtime metadata
  preview <account>     Read-only switch preview for humans and agents
  recovery              Read unfinished-switch and Herdr recovery metadata
  update [--check]      Update a known binary/Go install; defer Homebrew to brew
  version               Print the build version

Options:
  --json                Machine-readable output
  --state-dir PATH      Verso private state directory
  --codex-home PATH     Native Codex home (default CODEX_HOME or ~/.codex)
  --codex-bin PATH      Native Codex executable
  --allow-exhausted      Explicitly allow a target with exhausted quota
  --allow-no-snapshot    Allow switching if Herdr capture fails

Switches require an interactive human terminal. No agent --yes bypass.
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
	version := fs.Bool("version", false, "")
	shortVersion := fs.Bool("v", false, "")
	refresh := fs.Bool("refresh", false, "")
	checkUpdate := fs.Bool("check", false, "")
	allowExhausted := fs.Bool("allow-exhausted", false, "")
	allowNoSnapshot := fs.Bool("allow-no-snapshot", false, "")
	ordered, err := flagsFirst(fs, args)
	if err == nil {
		err = fs.Parse(ordered)
	}
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprint(a.Out, usage)
		return 0
	}
	if err != nil {
		return a.finish(response{Command: "usage"}, errors.New("invalid arguments; see verso --help"))
	}
	if *version || *shortVersion {
		return a.finish(response{Command: "version", Message: a.Version}, nil)
	}
	pos := fs.Args()
	if len(pos) == 0 {
		_, _ = fmt.Fprint(a.Out, usage)
		return 0
	}
	command := pos[0]
	pos = pos[1:]
	if command == "help" {
		_, _ = fmt.Fprint(a.Out, usage)
		return 0
	}
	if command == "version" && len(pos) == 0 {
		return a.finish(response{Command: command, Message: a.Version}, nil)
	}
	if command == "update" {
		return a.updateCommand(ctx, pos, *checkUpdate)
	}
	if command == "quota" {
		return a.quotaCommand(ctx, pos, *refresh)
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
	if command != "recovery" && command != "list" && command != "status" && command != "preview" {
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
	if command == "list" {
		return a.finish(r, nil)
	}
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
	}
	if a.json {
		e := json.NewEncoder(a.Out)
		e.SetIndent("", "  ")
		if e.Encode(r) != nil {
			return 1
		}
	} else {
		if r.Message != "" {
			_, _ = fmt.Fprintln(a.Out, r.Message)
		}
		if r.Command == "quota" {
			for _, account := range r.Accounts {
				_, _ = fmt.Fprintf(a.Out, "%q: %s\n", account.Alias, quotaText(r.Quotas[account.ID]))
			}
		}
		if r.Command == "list" {
			if len(r.Accounts) == 0 {
				_, _ = fmt.Fprintln(a.Out, "No accounts saved.")
			}
			for _, account := range r.Accounts {
				_, _ = fmt.Fprintf(a.Out, "%s  %q  %q  workspace=%q\n", account.ID, account.Alias, account.Email, account.AccountID)
			}
		}
		if r.Journal != nil {
			_, _ = fmt.Fprintf(a.Out, "Phase: %s\nPrevious: %q\nRequested: %q\n", r.Journal.Phase, r.Journal.From, r.Journal.Target)
		}
		if r.Switch != nil && r.Switch.RollbackAttempted {
			if r.Switch.RollbackSucceeded {
				_, _ = fmt.Fprintln(a.Out, "Rollback: previous account restored and verified.")
			} else {
				_, _ = fmt.Fprintln(a.Out, "Rollback: incomplete; inspect verso recovery before further changes.")
			}
		}
		if r.Plan != nil {
			_, _ = fmt.Fprintf(a.Out, "Daemon: %s\n", r.Plan.Daemon)
			for _, id := range r.Plan.Busy {
				_, _ = fmt.Fprintf(a.Out, "Blocking turn: %q\n", id)
			}
			for _, warning := range r.Plan.Warnings {
				_, _ = fmt.Fprintln(a.Out, "Warning:", warning)
			}
		}
		if r.Runtime != nil {
			_, _ = fmt.Fprintf(a.Out, "Daemon: %s\nCredential mode: %s\nSelected file identity: %q\n", r.Runtime.Daemon, r.Runtime.Config.CredentialStore, r.Runtime.Email)
			for _, thread := range r.Runtime.Busy {
				_, _ = fmt.Fprintf(a.Out, "Blocking turn: %s\n", thread)
			}
			for _, warning := range r.Runtime.Warnings {
				_, _ = fmt.Fprintf(a.Out, "Warning: %s\n", warning)
			}
		}
		if r.Target != nil {
			_, _ = fmt.Fprintf(a.Out, "Target: %q (%q)\n", r.Target.Alias, r.Target.Email)
		}
		if err != nil {
			_, _ = fmt.Fprintln(a.Err, "verso:", r.Error)
		}
	}
	if err != nil {
		return 1
	}
	return 0
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
