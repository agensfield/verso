package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	application "github.com/agensfield/verso/internal/app"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/codex"
	"github.com/agensfield/verso/internal/quota"
	"github.com/agensfield/verso/internal/switcher"
)

func (a *App) inspector() (codex.Inspector, error) {
	cwd := a.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return codex.Inspector{}, errors.New("cannot resolve startup working directory")
		}
	}
	resolver := a.CredentialResolver
	if resolver == nil {
		resolver = codex.NewLocalCredentialResolver(a.RunCommand)
	}
	env := a.Env
	if env == nil {
		env = os.Environ()
	}
	return codex.Inspector{LaunchEnvKnown: true, LaunchArgs: []string{"app-server", "--listen", "unix://"}, Home: a.CodexHome, Binary: a.Binary, Version: a.Version, Env: env, Run: a.RunCommand, CWD: cwd, Resolver: resolver}, nil
}
func (a *App) network() quota.Client {
	if client, ok := a.Auth.(quota.Client); ok {
		return client
	}
	return auth.NewClient(auth.Config{})
}
func (a *App) backend() (*application.Backend, error) {
	inspector, err := a.inspector()
	if err != nil {
		return nil, err
	}
	b := &application.Backend{Root: a.StateDir, Home: a.CodexHome, Runtime: &application.NativeRuntime{Inspector: inspector}, Auth: a.network()}
	for _, entry := range a.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && value != "" && (key == "HERDR_ENV" || key == "HERDR_SOCKET_PATH") {
			b.HerdrAvailable = true
		}
	}
	b.CaptureHerdr = func(ctx context.Context) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		run := a.RunCommand
		if run == nil {
			run = codex.RunCommand
		}
		return run(ctx, "herdr", "api", "snapshot")
	}
	b.Reauthenticate = func(ctx context.Context, account accounts.Account) ([]byte, error) {
		if err := confirm(ctx, a.In, a.Out, fmt.Sprintf("Reauthenticate %q before switching?", account.Alias)); err != nil {
			return nil, err
		}
		client := a.Auth
		if client == nil {
			client = auth.NewClient(auth.Config{})
		}
		return client.DeviceLogin(ctx, func(p auth.DevicePrompt) error {
			_, err := fmt.Fprintf(a.Out, "Open %s and enter code %s\n", p.VerificationURL, p.UserCode)
			return err
		})
	}
	return b, nil
}

func (a *App) switchAccount(ctx context.Context, args []string, allowExhausted, allowNoSnapshot bool) int {
	r := response{Command: "switch"}
	if len(args) > 1 {
		return a.finish(r, errors.New("usage: verso switch [account]"))
	}
	if err := a.requireHuman(); err != nil {
		return a.finish(r, err)
	}
	if !filepath.IsAbs(a.StateDir) || !filepath.IsAbs(a.CodexHome) {
		return a.finish(r, errors.New("state and Codex home paths must be absolute"))
	}
	run := a.RunCommand
	if run == nil {
		run = codex.RunCommand
	}
	binary := a.Binary
	if binary == "" {
		binary = "codex"
	}
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	raw, err := run(versionCtx, binary, "--version")
	cancel()
	if err != nil || !application.SupportedVersion(string(raw)) {
		return a.finish(r, errors.New("Codex version is unknown or below the alpha floor of 0.152.0"))
	}
	store, err := accounts.OpenReadOnly(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		return a.finish(r, err)
	}
	query := ""
	if len(args) == 1 {
		query = args[0]
	} else {
		saved, err := store.List()
		if err != nil {
			return a.finish(r, err)
		}
		if len(saved) == 0 {
			return a.finish(r, errors.New("no saved accounts; run verso add first"))
		}
		entries, active, _, err := a.fetchQuotas(ctx, saved, false)
		if err != nil {
			return a.finish(r, err)
		}
		saved, err = store.List()
		if err != nil {
			return a.finish(r, err)
		}
		if _, err := fmt.Fprintln(a.Out, a.humanHeading("Choose account")); err != nil {
			return a.finish(r, err)
		}
		if err := a.renderAccountCards(response{Accounts: saved, Quotas: entries, Active: active}, true); err != nil {
			return a.finish(r, err)
		}
		n, err := readAccountNumber(ctx, a.In, a.Out, len(saved))
		if err != nil {
			return a.finish(r, err)
		}
		query = saved[n-1].ID
	}
	target, err := store.Find(query)
	if err != nil {
		return a.finish(r, err)
	}
	r.Target = &target
	backend, err := a.backend()
	if err != nil {
		return a.finish(r, err)
	}
	result, err := (switcher.Engine{Backend: backend}).Execute(ctx, switcher.Request{Target: target.ID, AllowExhausted: allowExhausted, AllowNoSnapshot: allowNoSnapshot}, func(plan switcher.Plan) error {
		for _, warning := range plan.Warnings {
			_, _ = fmt.Fprintln(a.Out, "Warning:", warning)
		}
		if plan.Daemon == switcher.Running {
			_, _ = fmt.Fprintln(a.Out, "The managed daemon will stop and restart. Clients may reconnect or need reopening.")
		} else {
			_, _ = fmt.Fprintln(a.Out, "No central daemon is running. Close and reopen standalone Codex sessions to use the selected account.")
		}
		if allowNoSnapshot {
			_, _ = fmt.Fprintln(a.Out, "Proceeding is allowed even if Herdr recovery capture fails.")
		}
		return confirm(ctx, a.In, a.Out, fmt.Sprintf("Switch to %q?", target.Alias))
	})
	r.Switch = &result
	if err == nil {
		r.Message = "Account selected."
		if result.Changed && backend.Standalone() {
			r.Message = "Credential file updated. Close and reopen Codex to apply the selection; managed configuration may override local mode."
		}
		if !result.Changed {
			r.Message = "That account is already selected."
		}
	}
	return a.finish(r, err)
}

func readAccountNumber(ctx context.Context, in io.Reader, out io.Writer, count int) (int, error) {
	lines := make(chan string)
	done := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(in)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
		done <- scanner.Err()
	}()
	for {
		if _, err := fmt.Fprint(out, "Account number (Enter cancels): "); err != nil {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case line, ok := <-lines:
			if !ok {
				readErr := <-done
				if readErr != nil {
					return 0, readErr
				}
				return 0, errors.New("cancelled; no switch performed")
			}
			line = strings.TrimSpace(line)
			if line == "" {
				return 0, errors.New("cancelled; no switch performed")
			}
			n, parseErr := strconv.Atoi(line)
			if parseErr == nil && n >= 1 && n <= count {
				return n, nil
			}
			if _, err := fmt.Fprintf(out, "Choose a number from 1 to %d, or press Enter to cancel.\n", count); err != nil {
				return 0, err
			}
		}
	}
}
