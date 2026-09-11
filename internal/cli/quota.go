package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	application "github.com/agensfield/verso/internal/app"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/operation"
	"github.com/agensfield/verso/internal/quota"
	"github.com/agensfield/verso/internal/selection"
)

func (a *App) fetchQuotas(ctx context.Context, saved []accounts.Account, force bool) (map[string]quota.Entry, error) {
	release, err := operation.Lock(a.StateDir)
	if err != nil {
		return nil, err
	}
	defer release()
	inspector, err := a.inspector()
	if err != nil {
		return nil, err
	}
	observation, inspectErr := inspector.Inspect(ctx)
	active := accounts.ActiveIdentity{}
	if inspectErr == nil && application.FileSelectionAllowed(observation) {
		active = observation.SelectedFile
	}
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		return nil, err
	}
	client := a.network()
	service := quota.Service{CheckSelection: func() error {
		if !selection.Complete(active) {
			return nil
		} // service itself suppresses refresh for unknown selection
		_, now, err := selection.Read(a.CodexHome)
		if err != nil {
			return err
		}
		if !selection.Equal(now, active) {
			return selection.ErrChanged
		}
		return nil
	}, Root: a.StateDir, Store: store, Client: client, Active: active}
	service.ActiveUsage = func(ctx context.Context) (auth.Quota, error) {
		// Read the authoritative file, never the potentially stale saved copy, and
		// use Usage only. A failed active request does not authorize Verso refresh.
		raw, current, err := selection.Read(a.CodexHome)
		if err != nil {
			return auth.Quota{}, err
		}
		if !selection.Equal(current, active) {
			return auth.Quota{}, selection.ErrChanged
		}
		q, err := client.Usage(ctx, raw)
		if err != nil {
			return auth.Quota{}, err
		}
		_, after, err := selection.Read(a.CodexHome)
		if err != nil || !selection.Equal(after, active) {
			return auth.Quota{}, selection.ErrChanged
		}
		return q, nil
	}
	entries := make(map[string]quota.Entry, len(saved))
	// Sequential bounded requests suit the two-account alpha; no resident worker.
	for _, account := range saved {
		if err := ctx.Err(); err != nil {
			return entries, err
		}
		entry, err := service.Refresh(ctx, account.ID, force)
		if err != nil {
			return entries, err
		}
		entries[account.ID] = entry
	}
	return entries, nil
}

func quotaText(e quota.Entry) string {
	if e.LoginRequired {
		return "login needed"
	}
	if e.Quota == nil {
		if e.Warning != "" {
			return "unknown (" + e.Warning + ")"
		}
		return "unknown"
	}
	var windows []string
	for _, window := range []*auth.Window{e.Quota.Primary, e.Quota.Secondary} {
		if window == nil {
			continue
		}
		label := "usage unknown"
		if window.UsedPercent != nil {
			label = fmt.Sprintf("%.0f%% used", *window.UsedPercent)
		}
		if window.Window != nil {
			label = window.Window.String() + ": " + label
		}
		if window.ResetsAt != nil {
			label += "; resets " + window.ResetsAt.Local().Format("Jan 2 15:04 MST")
		}
		windows = append(windows, label)
	}
	if len(windows) == 0 {
		windows = []string{"quota details unavailable"}
	}
	result := strings.Join(windows, " | ")
	if e.Stale {
		result += " (stale)"
	}
	if !e.CheckedAt.IsZero() {
		result += "; observed " + e.CheckedAt.Local().Format(time.RFC3339)
	}
	if e.Warning != "" {
		result += "; " + e.Warning
	}
	return result
}

func (a *App) quotaCommand(ctx context.Context, args []string, force bool) int {
	r := response{Command: "quota"}
	if len(args) > 1 {
		return a.finish(r, errors.New("usage: verso quota [account] [--refresh]"))
	}
	if !filepath.IsAbs(a.StateDir) || !filepath.IsAbs(a.CodexHome) {
		return a.finish(r, errors.New("state and Codex home paths must be absolute"))
	}
	store, err := accounts.OpenReadOnly(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		return a.finish(r, err)
	}
	r.Accounts, err = store.List()
	if err != nil {
		return a.finish(r, err)
	}
	if len(args) == 1 {
		account, e := store.Find(args[0])
		if e != nil {
			return a.finish(r, e)
		}
		r.Accounts = []accounts.Account{account}
	}
	if len(r.Accounts) == 0 {
		r.Message = "No accounts saved."
		return a.finish(r, nil)
	}
	r.Quotas, err = a.fetchQuotas(ctx, r.Accounts, force)
	return a.finish(r, err)
}
