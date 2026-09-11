package cli

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/agensfield/verso/internal/accounts"
	application "github.com/agensfield/verso/internal/app"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/operation"
	"github.com/agensfield/verso/internal/quota"
	"github.com/agensfield/verso/internal/selection"
)

func (a *App) fetchQuotas(ctx context.Context, saved []accounts.Account, force bool) (map[string]quota.Entry, string, error) {
	release, err := operation.Lock(a.StateDir)
	if err != nil {
		return nil, "", err
	}
	defer release()
	inspector, err := a.inspector()
	if err != nil {
		return nil, "", err
	}
	observation, inspectErr := inspector.InspectSelection(ctx)
	active := accounts.ActiveIdentity{}
	if inspectErr == nil && application.FileSelectionAllowed(observation) {
		active = observation.SelectedFile
	}
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		return nil, "", err
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
			return auth.Quota{}, quota.ErrSelectionChanged
		}
		if !selection.Equal(current, active) {
			return auth.Quota{}, quota.ErrSelectionChanged
		}
		q, err := client.Usage(ctx, raw)
		if err != nil {
			return auth.Quota{}, err
		}
		_, after, err := selection.Read(a.CodexHome)
		if err != nil || !selection.Equal(after, active) {
			return auth.Quota{}, quota.ErrSelectionChanged
		}
		return q, nil
	}
	activeID := ""
	for _, account := range saved {
		if active.Known && account.UserID == active.UserID && account.AccountID == active.AccountID {
			activeID = account.ID
		}
	}
	entries := make(map[string]quota.Entry, len(saved))
	// Sequential bounded requests suit the two-account alpha; no resident worker.
	for _, account := range saved {
		if err := ctx.Err(); err != nil {
			return entries, activeID, err
		}
		entry, err := service.Refresh(ctx, account.ID, force)
		if err != nil {
			return entries, activeID, err
		}
		entries[account.ID] = entry
	}
	return entries, activeID, nil
}

func (a *App) listCommand(ctx context.Context, args []string, cached bool) int {
	r := response{Command: "list", Cached: cached}
	if len(args) > 1 {
		return a.finish(r, errors.New("usage: verso list [account] [--cached]"))
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
	if cached {
		r.Quotas = make(map[string]quota.Entry, len(r.Accounts))
		for _, account := range r.Accounts {
			entry, _, cacheErr := (quota.Service{Root: a.StateDir}).Cached(account.ID)
			if cacheErr != nil {
				return a.finish(r, cacheErr)
			}
			r.Quotas[account.ID] = entry
		}
	} else {
		r.Quotas, r.Active, err = a.fetchQuotas(ctx, r.Accounts, true)
		if err != nil {
			// The selection proof is no longer current, so do not render its badge.
			r.Active = ""
		}
		if err == nil {
			// Inactive credential refresh can update list-safe account metadata.
			if len(args) == 1 {
				var account accounts.Account
				account, err = store.Find(r.Accounts[0].ID)
				if err == nil {
					r.Accounts = []accounts.Account{account}
				}
			} else {
				r.Accounts, err = store.List()
			}
		}
	}
	return a.finish(r, err)
}
