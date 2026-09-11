package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
)

type deviceFunc func(context.Context, func(auth.DevicePrompt) error) ([]byte, error)

func (f deviceFunc) DeviceLogin(ctx context.Context, prompt func(auth.DevicePrompt) error) ([]byte, error) {
	return f(ctx, prompt)
}

func TestDeviceEnrollmentOnlyWritesVersoStore(t *testing.T) {
	a, out, errOut := appFixture(t)
	if err := os.MkdirAll(a.CodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("opaque native state must remain untouched")
	file := filepath.Join(a.CodexHome, "auth.json")
	if err := os.WriteFile(file, original, 0600); err != nil {
		t.Fatal(err)
	}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"new@example.test","https://api.openai.com/auth":{"chatgpt_user_id":"new-user","chatgpt_account_id":"new-workspace"}}`))
	a.Auth = deviceFunc(func(_ context.Context, prompt func(auth.DevicePrompt) error) ([]byte, error) {
		if err := prompt(auth.DevicePrompt{VerificationURL: "https://example.test/device", UserCode: "HUMAN-CODE"}); err != nil {
			return nil, err
		}
		return []byte(`{"tokens":{"id_token":"e30.` + claims + `.sig","access_token":"NEVER-PRINT","account_id":"new-workspace"}}`), nil
	})
	if code := a.Run(context.Background(), []string{"add", "work"}); code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != string(original) {
		t.Fatal("modified native auth")
	}
	store, err := accounts.OpenReadOnly(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Find("work")
	if err != nil || acc.AccountID != "new-workspace" {
		t.Fatalf("%+v %v", acc, err)
	}
	if !strings.Contains(out.String(), "HUMAN-CODE") || strings.Contains(out.String(), "NEVER-PRINT") || strings.Contains(out.String(), claims) {
		t.Fatal("incorrect enrollment output")
	}
}

func TestCancelledEnrollmentSavesNoAccount(t *testing.T) {
	a, _, _ := appFixture(t)
	a.Auth = deviceFunc(func(context.Context, func(auth.DevicePrompt) error) ([]byte, error) { return nil, context.Canceled })
	if a.Run(context.Background(), []string{"add"}) == 0 {
		t.Fatal("cancel succeeded")
	}
	store, err := accounts.OpenReadOnly(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.List()
	if err != nil || len(saved) != 0 {
		t.Fatalf("%v %v", saved, err)
	}
	if _, err := os.Stat(a.CodexHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created native home")
	}
}

func TestJSONEnrollmentDoesNotStartDeviceFlow(t *testing.T) {
	a, _, _ := appFixture(t)
	a.Auth = deviceFunc(func(context.Context, func(auth.DevicePrompt) error) ([]byte, error) {
		t.Fatal("device flow started")
		return nil, nil
	})
	if a.Run(context.Background(), []string{"add", "--json"}) == 0 {
		t.Fatal("expected human-output requirement")
	}
	if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
		t.Fatal("created state")
	}
}
