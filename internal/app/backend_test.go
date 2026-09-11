package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/codex"
	"github.com/agensfield/verso/internal/operation"
	"github.com/agensfield/verso/internal/selection"
	"github.com/agensfield/verso/internal/switcher"
)

type fakeRuntime struct {
	home               string
	running, busy      bool
	pid, stops, starts int
	failFirstStart     bool
}

func (f *fakeRuntime) Inspect(context.Context) (codex.Observation, error) {
	_, identity, err := selection.Read(f.home)
	if err != nil {
		return codex.Observation{}, err
	}
	o := codex.Observation{Daemon: switcher.Stopped, SelectedFile: identity, Credential: codex.CredentialProof{Status: codex.CredentialFileSelected, StartupCWD: f.home}}
	if f.running {
		o.Daemon = switcher.Running
		o.Record = &codex.ProcessRecord{PID: f.pid, StartTime: fmt.Sprintf("birth-%d", f.pid)}
	}
	if f.busy {
		o.Busy = []string{"active-thread"}
	}
	return o, nil
}
func (f *fakeRuntime) Stop(context.Context, codex.ProcessRecord) error {
	f.stops++
	f.running = false
	return nil
}
func (f *fakeRuntime) Start(context.Context, string) error {
	f.starts++
	if f.failFirstStart && f.starts == 1 {
		return errors.New("synthetic start failure")
	}
	f.running = true
	f.pid++
	return nil
}
func (f *fakeRuntime) Verify(_ context.Context, old codex.ProcessRecord, selected codex.NativeSelection) error {
	if !f.running || f.pid == old.PID {
		return errors.New("not a fresh process")
	}
	now, err := codex.ReadNativeSelection(f.home)
	if err != nil {
		return err
	}
	if now != selected {
		return errors.New("wrong native selection")
	}
	return nil
}

type fakeAuth struct{}

func (fakeAuth) Refresh(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("unexpected fixture refresh")
}
func (fakeAuth) Usage(context.Context, []byte) (auth.Quota, error) {
	exhausted := false
	return auth.Quota{Exhausted: &exhausted}, nil
}
func native(account string) []byte {
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":"same@example.test","https://api.openai.com/auth":{"chatgpt_user_id":"user","chatgpt_account_id":%q}}`, account)))
	return []byte(fmt.Sprintf(`{"opaque":{"retain":true},"tokens":{"id_token":"e30.%s.sig","access_token":"synthetic-only","account_id":%q}}`, claims, account))
}
func backendFixture(t *testing.T, running bool) (*Backend, *fakeRuntime, accounts.Account) {
	t.Helper()
	root := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), native("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "history.jsonl"), []byte("history untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := accounts.Open(filepath.Join(root, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := accounts.ParseNativeAuth(native("b"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Save(parsed, "work")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{home: home, running: running, pid: 123}
	return &Backend{Root: root, Home: home, Runtime: runtime, Auth: fakeAuth{}}, runtime, target
}
func TestStandaloneTransactionSavesOutgoingAndLeavesHistory(t *testing.T) {
	b, r, target := backendFixture(t, false)
	out, err := (switcher.Engine{Backend: b}).Execute(context.Background(), switcher.Request{Target: target.ID}, func(switcher.Plan) error { return nil })
	if err != nil || !out.Changed || out.Active != target.ID || r.starts != 0 || r.stops != 0 {
		t.Fatalf("%+v %v %+v", out, err, r)
	}
	_, selected, err := selection.Read(b.Home)
	if err != nil || selected.AccountID != "b" {
		t.Fatal("wrong selected account")
	}
	store, _ := b.store(false)
	saved, _ := store.List()
	if len(saved) != 2 {
		t.Fatal("outgoing account not saved")
	}
	history, _ := os.ReadFile(filepath.Join(b.Home, "history.jsonl"))
	if string(history) != "history untouched" {
		t.Fatal("history changed")
	}
	cp, err := (operation.Journal{Root: b.Root}).Read()
	if err != nil || cp != nil {
		t.Fatalf("journal: %+v %v", cp, err)
	}
}
func TestFailedTargetStartRestoresUnsavedOutgoingAccount(t *testing.T) {
	b, r, target := backendFixture(t, true)
	r.failFirstStart = true
	out, err := (switcher.Engine{Backend: b}).Execute(context.Background(), switcher.Request{Target: target.ID}, func(switcher.Plan) error { return nil })
	if err == nil || !out.RollbackSucceeded || !out.ActiveKnown || !r.running {
		t.Fatalf("%+v %v %+v", out, err, r)
	}
	_, identity, _ := selection.Read(b.Home)
	if identity.AccountID != "a" {
		t.Fatal("rollback failed to restore outgoing account")
	}
}
func TestCancellationAndBusyLeaveNativeAuthUntouched(t *testing.T) {
	for _, busy := range []bool{false, true} {
		b, r, target := backendFixture(t, true)
		r.busy = busy
		before, _ := os.ReadFile(filepath.Join(b.Home, "auth.json"))
		_, err := (switcher.Engine{Backend: b}).Execute(context.Background(), switcher.Request{Target: target.ID}, func(switcher.Plan) error { return errors.New("human cancelled") })
		after, _ := os.ReadFile(filepath.Join(b.Home, "auth.json"))
		if err == nil || string(before) != string(after) || r.stops != 0 || r.starts != 0 {
			t.Fatal("refusal changed runtime or auth")
		}
	}
}
func TestExternalIdentityChangeAfterSaveIsNotOverwritten(t *testing.T) {
	b, _, target := backendFixture(t, false)
	state, err := b.Inspect(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SaveActive(context.Background(), state.Active); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Home, "auth.json"), native("external"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.Activate(context.Background(), target.ID); err != selection.ErrChanged {
		t.Fatal(err)
	}
	_, identity, _ := selection.Read(b.Home)
	if identity.AccountID != "external" {
		t.Fatal("overwrote external account change")
	}
}
func TestStatedVersionFloor(t *testing.T) {
	for _, s := range []string{"codex-cli 0.152.0", "codex-cli 0.154.0", "1.0.0"} {
		if !SupportedVersion(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"codex-cli 0.151.9", "unrecognized", "0.15.2"} {
		if SupportedVersion(s) {
			t.Fatal(s)
		}
	}
}

type loginFailure struct{}

func (loginFailure) Usage(context.Context, []byte) (auth.Quota, error) {
	return auth.Quota{}, auth.ErrLoginRequired
}
func (loginFailure) Refresh(context.Context, []byte) ([]byte, error) {
	return nil, auth.ErrLoginRequired
}
func TestUnsuccessfulReauthNeverStopsOutgoingRuntime(t *testing.T) {
	b, r, target := backendFixture(t, true)
	b.Auth = loginFailure{}
	prompted := false
	b.Reauthenticate = func(context.Context, accounts.Account) ([]byte, error) {
		prompted = true
		_, identity, _ := selection.Read(b.Home)
		if !r.running || identity.AccountID != "a" {
			t.Fatal("reauth happened after stop/activation")
		}
		return native("b"), nil
	}
	_, err := (switcher.Engine{Backend: b}).Execute(context.Background(), switcher.Request{Target: target.ID}, func(switcher.Plan) error { return nil })
	if err == nil || !prompted || r.stops != 0 || !r.running {
		t.Fatalf("%v %+v", err, r)
	}
}
