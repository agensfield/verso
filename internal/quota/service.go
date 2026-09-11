// Package quota handles demand-driven observations. Callers serialize writes
// with the Verso operation lock; previews call Cached and never Refresh.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/operation"
)

type Client interface {
	Refresh(context.Context, []byte) ([]byte, error)
	Usage(context.Context, []byte) (auth.Quota, error)
}

type Entry struct {
	Quota         *auth.Quota `json:"quota,omitempty"`
	CheckedAt     time.Time   `json:"checked_at"`
	AttemptedAt   time.Time   `json:"attempted_at"`
	Stale         bool        `json:"stale"`
	LoginRequired bool        `json:"login_required,omitempty"`
	Warning       string      `json:"warning,omitempty"`
}

var ErrSelectionChanged = errors.New("selected native identity changed during quota inspection")

type Service struct {
	// CheckSelection is an instantaneous guard, not a lock against Codex writers.
	CheckSelection func() error
	Root           string
	Store          *accounts.Store
	Client         Client
	Now            func() time.Time
	// Active is the selected native identity. Unknown forbids token refresh.
	Active accounts.ActiveIdentity
	// ActiveUsage delegates active-account observation to Codex, which owns
	// active credential refresh. Never use the saved account copy for this.
	ActiveUsage func(context.Context) (auth.Quota, error)
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s Service) Cached(id string) (Entry, bool, error) {
	all, err := s.readCache()
	e, ok := all[id]
	if ok && e.Quota != nil && (s.now().Sub(e.CheckedAt) >= time.Minute || s.now().Before(e.CheckedAt)) {
		e.Stale = true
	}
	return e, ok, err
}

func (s Service) readCache() (map[string]Entry, error) {
	entries := map[string]Entry{}
	name := filepath.Join(s.Root, "quota.json")
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4<<20 {
		return nil, errors.New("cannot safely read quota cache")
	}
	raw, err := os.ReadFile(name)
	if err != nil || json.Unmarshal(raw, &entries) != nil {
		return nil, errors.New("invalid quota cache")
	}
	return entries, nil
}

// Refresh reuses observations for one minute unless explicitly refreshed.
// Failure retains stale data with a warning, never fabricating zero usage.
func (s Service) Refresh(ctx context.Context, id string, force bool) (Entry, error) {
	if s.CheckSelection != nil && s.CheckSelection() != nil {
		return Entry{}, ErrSelectionChanged
	}
	account, err := s.Store.Find(id)
	if err != nil {
		return Entry{}, err
	}
	entries, err := s.readCache()
	if err != nil {
		return Entry{}, err
	}
	previous := entries[account.ID]
	age := s.now().Sub(previous.AttemptedAt)
	if !force && !previous.AttemptedAt.IsZero() && age >= 0 && age < time.Minute {
		return previous, nil
	}
	e := previous
	e.AttemptedAt = s.now()
	e.Warning = ""
	e.LoginRequired = false
	var q auth.Quota
	known := s.Active.Known && ((s.Active.UserID == "") == (s.Active.AccountID == ""))
	active := known && s.Active.UserID == account.UserID && s.Active.AccountID == account.AccountID
	switch {
	case !known:
		err = errors.New("selected account is unknown; credential refresh skipped")
	case active:
		if s.ActiveUsage == nil {
			err = errors.New("active quota is unavailable; Codex owns active credential refresh")
		} else {
			q, err = s.ActiveUsage(ctx)
		}
	default:
		q, err = s.inactive(ctx, account)
	}
	if errors.Is(err, ErrSelectionChanged) {
		return previous, err
	}
	if err == nil {
		e.Quota = &q
		e.CheckedAt = s.now()
		e.Stale = false
	} else {
		e.Stale = e.Quota != nil
		// Only package-owned, sanitized classifications enter persistent metadata.
		e.Warning = "quota unavailable; previous observation may be stale"
		if errors.Is(err, auth.ErrLoginRequired) || errors.Is(err, auth.ErrInvalidAuth) {
			e.LoginRequired = true
			e.Warning = "reauthentication needed"
		}
		if !known {
			e.Warning = "selected account unknown; credential refresh skipped"
		}
		if active {
			e.LoginRequired = false
			e.Warning = "active quota unavailable; Codex owns credential refresh"
		}
	}
	entries[account.ID] = e
	raw, marshalErr := json.Marshal(entries)
	if marshalErr != nil {
		return e, marshalErr
	}
	if writeErr := operation.AtomicWrite(s.Root, "quota.json", raw); writeErr != nil {
		return e, writeErr
	}
	return e, nil // per-account network failures are displayed, not a failed list.
}

func (s Service) inactive(ctx context.Context, account accounts.Account) (auth.Quota, error) {
	if s.Client == nil {
		return auth.Quota{}, errors.New("quota client unavailable")
	}
	raw, err := s.Store.Credentials(account.ID)
	if err != nil {
		return auth.Quota{}, err
	}
	expired, expiryErr := auth.AccessTokenExpired(raw, s.now())
	refreshed := false
	if expiryErr == nil && expired {
		raw, err = s.refresh(ctx, account, raw)
		if err != nil {
			return auth.Quota{}, err
		}
		refreshed = true
	}
	q, err := s.Client.Usage(ctx, raw)
	if errors.Is(err, auth.ErrLoginRequired) && !refreshed {
		raw, err = s.refresh(ctx, account, raw)
		if err != nil {
			return auth.Quota{}, err
		}
		return s.Client.Usage(ctx, raw)
	}
	return q, err
}

func (s Service) refresh(ctx context.Context, account accounts.Account, raw []byte) ([]byte, error) {
	if s.CheckSelection != nil && s.CheckSelection() != nil {
		return nil, ErrSelectionChanged
	}
	next, err := s.Client.Refresh(ctx, raw)
	if err != nil {
		return nil, err
	}
	parsed, err := accounts.ParseNativeAuth(next)
	if err != nil {
		return nil, err
	}
	if _, err = s.Store.UpdateCredentials(account.ID, parsed); err != nil {
		return nil, err
	}
	return next, nil
}
