package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/operation"
)

func (a *App) add(ctx context.Context, args []string) int {
	r := response{Command: "add"}
	if len(args) > 1 {
		return a.finish(r, errors.New("usage: verso add [alias]"))
	}
	if a.json {
		return a.finish(r, errors.New("device authorization requires human output; omit --json"))
	}
	if !filepath.IsAbs(a.StateDir) {
		return a.finish(r, errors.New("state directory must be absolute"))
	}
	alias := ""
	if len(args) == 1 {
		alias = args[0]
	}
	// Acquire before authorizing: another Verso mutation must not begin while
	// the human completes enrollment. Codex's own state is never touched here.
	release, err := operation.Lock(a.StateDir)
	if err != nil {
		return a.finish(r, err)
	}
	defer release()
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		return a.finish(r, err)
	}
	client := a.Auth
	if client == nil {
		client = auth.NewClient(auth.Config{})
	}
	raw, err := client.DeviceLogin(ctx, func(prompt auth.DevicePrompt) error {
		// The device code is deliberately shown only in this human-initiated flow,
		// never in stored metadata, logs, JSON results, or errors.
		_, e := fmt.Fprintf(a.Out, "Open %s and enter code %s\n", prompt.VerificationURL, prompt.UserCode)
		if e == nil && !prompt.ExpiresAt.IsZero() {
			_, e = fmt.Fprintf(a.Out, "This code expires at %s. Press Ctrl-C to cancel.\n", prompt.ExpiresAt.Local().Format("15:04 MST"))
		}
		return e
	})
	if err != nil {
		return a.finish(r, err)
	}
	native, err := accounts.ParseNativeAuth(raw)
	if err != nil {
		return a.finish(r, err)
	}
	account, err := store.Save(native, alias)
	if err != nil {
		return a.finish(r, err)
	}
	r.Target = &account
	r.Message = "Account saved. The selected Codex account has not changed. Run `verso list` to inspect saved accounts."
	return a.finish(r, nil)
}
