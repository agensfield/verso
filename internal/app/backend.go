package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/codex"
	"github.com/agensfield/verso/internal/herdr"
	"github.com/agensfield/verso/internal/operation"
	"github.com/agensfield/verso/internal/quota"
	"github.com/agensfield/verso/internal/selection"
	"github.com/agensfield/verso/internal/switcher"
)

type Backend struct {
	allowed        []accounts.ActiveIdentity
	stoppedOnce    bool
	Root, Home     string
	Runtime        Runtime
	Auth           quota.Client
	HerdrAvailable bool
	CaptureHerdr   func(context.Context) ([]byte, error)
	Reauthenticate func(context.Context, accounts.Account) ([]byte, error)
	Now            func() time.Time
	observation    codex.Observation
	previous       codex.ProcessRecord
	startupCWD     string
	installed      codex.NativeSelection
}

func (b *Backend) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}
func (b *Backend) store(write bool) (*accounts.Store, error) {
	root := filepath.Join(b.Root, "accounts")
	if write {
		return accounts.Open(root)
	}
	return accounts.OpenReadOnly(root)
}
func (b *Backend) Lock(context.Context) (func(), error) { return operation.Lock(b.Root) }
func (b *Backend) Unfinished(context.Context) (bool, error) {
	cp, err := (operation.Journal{Root: b.Root}).Read()
	return cp != nil, err
}
func (b *Backend) WriteCheckpoint(_ context.Context, cp switcher.Checkpoint) error {
	return (operation.Journal{Root: b.Root}).Write(cp)
}
func (b *Backend) ClearCheckpoint(context.Context) error {
	return (operation.Journal{Root: b.Root}).Clear()
}

func (b *Backend) Inspect(ctx context.Context, target string) (switcher.Inspection, error) {
	state := switcher.Inspection{Daemon: switcher.Unknown, QuotaUnknown: true, Herdr: b.HerdrAvailable}
	o, err := b.Runtime.Inspect(ctx)
	b.observation = o
	state.Daemon, state.Busy, state.Warnings = o.Daemon, o.Busy, o.Warnings
	if err != nil {
		return state, err
	}
	state.FileBacked = o.Credential.Status == codex.CredentialFileSelected
	if !state.FileBacked {
		return state, fmt.Errorf("native credential configuration is unproven: %s", o.Credential.Reason)
	}
	_, identity, err := selection.Read(b.Home)
	if err != nil {
		return state, err
	}
	if !selection.Equal(identity, o.SelectedFile) {
		return state, selection.ErrChanged
	}
	store, err := b.store(false)
	if err != nil {
		return state, err
	}
	saved, err := store.List()
	if err != nil {
		return state, err
	}
	state.ActiveKnown = selection.Complete(identity)
	state.Active = identityHandle(identity, saved)
	cache := quota.Service{Root: b.Root, Now: b.Now}
	entry, ok, err := cache.Cached(target)
	if err != nil {
		return state, err
	}
	if ok && !entry.Stale && entry.Quota != nil && entry.Quota.Exhausted != nil {
		state.QuotaUnknown = false
		state.Exhausted = *entry.Quota.Exhausted
	}
	if state.QuotaUnknown {
		state.Warnings = append(state.Warnings, "target quota is unknown or stale")
	}
	return state, nil
}

// A native identity may not yet be enrolled. A credential-free handle preserves
// its identity through approval; SaveActive enrolls the final post-stop document.
func identityHandle(identity accounts.ActiveIdentity, saved []accounts.Account) string {
	if identity.UserID == "" && identity.AccountID == "" {
		return ""
	}
	for _, a := range saved {
		if a.UserID == identity.UserID && a.AccountID == identity.AccountID {
			return a.ID
		}
	}
	return fmt.Sprintf("native:%x", sha256.Sum256([]byte(identity.UserID+"\x00"+identity.AccountID)))
}
func resolveHandle(store *accounts.Store, handle string) (accounts.Account, error) {
	if account, err := store.Find(handle); err == nil {
		return account, nil
	}
	saved, err := store.List()
	if err != nil {
		return accounts.Account{}, err
	}
	for _, account := range saved {
		identity := accounts.ActiveIdentity{Known: true, UserID: account.UserID, AccountID: account.AccountID}
		if identityHandle(identity, nil) == handle {
			return account, nil
		}
	}
	return accounts.Account{}, accounts.ErrNotFound
}

func (b *Backend) PrepareTarget(ctx context.Context, target string) error {
	store, err := b.store(true)
	if err != nil {
		return err
	}
	account, err := store.Find(target)
	if err != nil {
		return err
	}
	service := quota.Service{Root: b.Root, Store: store, Client: b.Auth, Now: b.Now, Active: b.observation.SelectedFile}
	entry, err := service.Refresh(ctx, account.ID, true)
	if err != nil {
		return err
	}
	raw, err := store.Credentials(account.ID)
	if err != nil {
		return err
	}
	expired, expiryErr := auth.AccessTokenExpired(raw, b.now())
	if entry.LoginRequired || (expiryErr == nil && expired) {
		if b.Reauthenticate == nil {
			return errors.New("target account needs reauthentication before switching")
		}
		raw, err = b.Reauthenticate(ctx, account)
		if err != nil {
			return err
		}
		parsed, err := accounts.ParseNativeAuth(raw)
		if err != nil {
			return err
		}
		if _, err = store.UpdateCredentials(account.ID, parsed); err != nil {
			return err
		}
		_, err = service.Refresh(ctx, account.ID, true)
		return err
	}
	return nil
}
func (b *Backend) Snapshot(ctx context.Context) error {
	if b.CaptureHerdr == nil {
		return errors.New("Herdr capture is unavailable")
	}
	raw, err := b.CaptureHerdr(ctx)
	if err != nil {
		return errors.New("Herdr capture failed")
	}
	return herdr.Capture(b.Root, raw, b.now())
}
func (b *Backend) Stop(ctx context.Context) error {
	current, err := b.Runtime.Inspect(ctx)
	if err != nil {
		return err
	}
	if current.Daemon == switcher.Stopped {
		return nil
	}
	if current.Record == nil {
		return errors.New("managed process identity is unavailable")
	}
	if !b.stoppedOnce && (b.observation.Record == nil || *current.Record != *b.observation.Record) {
		return errors.New("managed daemon changed after approval")
	}
	b.stoppedOnce = true
	b.previous = *current.Record
	if b.startupCWD == "" {
		b.startupCWD = current.Credential.StartupCWD
	}
	return b.Runtime.Stop(ctx, *current.Record)
}
func (b *Backend) SaveActive(_ context.Context, handle string) error {
	raw, identity, err := selection.Read(b.Home)
	if err != nil {
		return err
	}
	store, err := b.store(true)
	if err != nil {
		return err
	}
	saved, err := store.List()
	if err != nil {
		return err
	}
	if identityHandle(identity, saved) != handle {
		return selection.ErrChanged
	}
	b.allowed = []accounts.ActiveIdentity{identity}
	if identity.UserID == "" {
		return nil
	}
	native, err := accounts.ParseNativeAuth(raw)
	if err != nil {
		return err
	}
	_, err = store.Save(native, "")
	return err
}
func (b *Backend) Activate(_ context.Context, handle string) error {
	_, current, err := selection.Read(b.Home)
	if err != nil {
		return err
	}
	allowed := false
	for _, expected := range b.allowed {
		if selection.Equal(current, expected) {
			allowed = true
		}
	}
	if !allowed {
		return selection.ErrChanged
	}
	if handle == "" {
		err = selection.Clear(b.Home, current)
	} else {
		store, e := b.store(false)
		if e != nil {
			return e
		}
		account, e := resolveHandle(store, handle)
		if e != nil {
			return e
		}
		raw, e := store.Credentials(account.ID)
		if e != nil {
			return e
		}
		b.allowed = append(b.allowed, accounts.ActiveIdentity{Known: true, UserID: account.UserID, AccountID: account.AccountID})
		err = selection.Install(b.Home, raw, current)
	}
	if err != nil {
		return err
	}
	b.installed, err = codex.ReadNativeSelection(b.Home)
	return err
}
func (b *Backend) Start(ctx context.Context) error { return b.Runtime.Start(ctx, b.startupCWD) }
func (b *Backend) Verify(ctx context.Context, handle string, hadDaemon bool) error {
	_, identity, err := selection.Read(b.Home)
	if err != nil {
		return err
	}
	store, err := b.store(false)
	if err != nil {
		return err
	}
	saved, err := store.List()
	if err != nil {
		return err
	}
	if handle != "" {
		account, err := resolveHandle(store, handle)
		if err != nil {
			return err
		}
		if account.UserID != identity.UserID || account.AccountID != identity.AccountID {
			return selection.ErrChanged
		}
	} else if identityHandle(identity, saved) != "" {
		return selection.ErrChanged
	}
	if hadDaemon {
		return b.Runtime.Verify(ctx, b.previous, b.installed)
	}
	return nil
}
