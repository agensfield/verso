package cli

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	application "github.com/agensfield/verso/internal/app"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/operation"
	"github.com/agensfield/verso/internal/quota"
	"github.com/agensfield/verso/internal/selection"
)

func (a *App) fetchQuotas(ctx context.Context, saved []accounts.Account, force bool) (map[string]quota.Entry, string, string, int, error) {
	release, err := operation.Lock(a.StateDir)
	if err != nil {
		return nil, "", "unknown", 0, err
	}
	defer release()
	inspector, err := a.inspector()
	if err != nil {
		return nil, "", "unknown", 0, err
	}
	observation, inspectErr := inspector.InspectSelection(ctx)
	active := accounts.ActiveIdentity{}
	selectionStatus := "unknown"
	if inspectErr == nil && application.FileSelectionAllowed(observation) {
		active = observation.SelectedFile
		selectionStatus = "verified_unmatched"
		if active.Known && active.UserID == "" && active.AccountID == "" {
			selectionStatus = "no_login"
		}
	}
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		return nil, "", selectionStatus, 0, err
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
			selectionStatus = "verified"
		}
	}
	entries := make(map[string]quota.Entry, len(saved))
	attempted := 0
	// Sequential bounded requests suit the two-account alpha; no resident worker.
	for index, account := range saved {
		if err := ctx.Err(); err != nil {
			return entries, activeID, selectionStatus, attempted, err
		}
		a.progress("Checking usage: %s (%d/%d)", accountChoiceName(account), index+1, len(saved))
		attempted++
		entry, err := service.Refresh(ctx, account.ID, force)
		if err != nil {
			return entries, activeID, selectionStatus, attempted, err
		}
		entries[account.ID] = entry
	}
	if err := ctx.Err(); err != nil {
		return entries, activeID, selectionStatus, attempted, err
	}
	return entries, activeID, selectionStatus, attempted, nil
}

func (a *App) listCommand(ctx context.Context, args []string, cached bool) int {
	r := response{Command: "list", Cached: cached, Accounts: []accounts.Account{}, Quotas: map[string]quota.Entry{}, QuotaInfo: map[string]quotaWire{}, Selection: &selectionMetadata{Status: "not_inspected"}}
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
	var issues []accounts.AccountIssue
	r.Accounts, issues, err = store.ListPartial()
	if err != nil {
		return a.finish(r, err)
	}
	r.Inventory = inventoryResult(issues, nil)
	if len(args) == 1 {
		account, e := findInspectionAccount(store, r.Accounts, issues, args[0])
		if e != nil {
			r.Accounts = []accounts.Account{}
			return a.finish(r, e)
		}
		r.Accounts = []accounts.Account{account}
	}
	if len(r.Accounts) == 0 {
		if len(issues) > 0 {
			r.Message = "No healthy saved accounts could be listed. Inspect account_inventory in JSON before changing account state."
		} else {
			r.Message = "No accounts saved. Import the current login with `verso import personal`, or add another with `verso add work`."
		}
		source := "refresh"
		if cached {
			source = "cache"
		}
		r.QuotaState = &quotaMetadata{Source: source, Complete: true}
		return a.finish(r, nil)
	}
	if cached || len(issues) > 0 {
		if len(issues) > 0 && !cached {
			r.Cached = true
			r.Message = "Account inventory is incomplete. Usage refresh was skipped; showing healthy saved accounts with cached observations."
		}
		r.Quotas = make(map[string]quota.Entry, len(r.Accounts))
		for _, account := range r.Accounts {
			entry, _, cacheErr := (quota.Service{Root: a.StateDir}).Cached(account.ID)
			if cacheErr != nil {
				return a.finish(r, cacheErr)
			}
			r.Quotas[account.ID] = entry
		}
	} else {
		var selectionStatus string
		var attempted int
		r.Quotas, r.Active, selectionStatus, attempted, err = a.fetchQuotas(ctx, r.Accounts, true)
		r.Selection.Status = selectionStatus
		if err != nil {
			// The selection proof is no longer current, so do not render its badge.
			r.Active = ""
			r.Selection.Status = "unknown"
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
		r.QuotaState = summarizeQuotas(r.Accounts, r.Quotas, attempted, true)
	}
	if r.Cached {
		r.QuotaState = summarizeQuotas(r.Accounts, r.Quotas, 0, false)
	}
	r.QuotaInfo = quotaWires(r.Accounts, r.Quotas)
	return a.finish(r, err)
}

func summarizeQuotas(saved []accounts.Account, entries map[string]quota.Entry, attempted int, refresh bool) *quotaMetadata {
	result := &quotaMetadata{Source: "cache", Complete: true}
	result.Attempted = attempted
	successful := 0
	for _, account := range saved {
		entry, found := entries[account.ID]
		if found && entry.Quota != nil {
			result.Available++
		}
		if found && entry.Warning == "" {
			successful++
		}
	}
	if refresh {
		result.Source = "refresh"
		result.Failed = max(0, attempted-successful)
		result.Skipped = max(0, len(saved)-attempted)
		result.Complete = result.Failed == 0 && result.Skipped == 0
	}
	return result
}

func quotaWires(saved []accounts.Account, entries map[string]quota.Entry) map[string]quotaWire {
	result := make(map[string]quotaWire, len(saved))
	for _, account := range saved {
		entry := entries[account.ID]
		wire := quotaWire{
			Primary: windowToWire(nil), Secondary: windowToWire(nil), Plan: nil, Exhausted: nil,
			ObservedAt: timeOrNil(time.Time{}), CheckedAt: timeOrNil(entry.CheckedAt), AttemptedAt: timeOrNil(entry.AttemptedAt),
			Stale: entry.Stale, LoginRequired: entry.LoginRequired, Warning: entry.Warning,
		}
		if entry.Quota != nil {
			wire.Primary = windowToWire(entry.Quota.Primary)
			wire.Secondary = windowToWire(entry.Quota.Secondary)
			wire.Plan = entry.Quota.Plan
			wire.Exhausted = entry.Quota.Exhausted
			wire.ObservedAt = timeOrNil(entry.Quota.ObservedAt)
		}
		result[account.ID] = wire
	}
	return result
}

func windowToWire(window *auth.Window) *windowWire {
	if window == nil {
		return nil
	}
	result := &windowWire{UsedPercent: window.UsedPercent, ResetsAt: window.ResetsAt}
	if window.Window != nil {
		seconds := int64((*window.Window) / time.Second)
		result.WindowSeconds = &seconds
	}
	if window.ResetAfter != nil {
		seconds := int64((*window.ResetAfter) / time.Second)
		result.ResetInSeconds = &seconds
	}
	return result
}

func timeOrNil(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
