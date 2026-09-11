package selection

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agensfield/verso/internal/accounts"
)

func native(account string) []byte {
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":"same@example.test","https://api.openai.com/auth":{"chatgpt_user_id":"user","chatgpt_account_id":%q}}`, account)))
	return []byte(fmt.Sprintf(`{"unknown":{"keep":true},"tokens":{"id_token":"e30.%s.sig","access_token":"synthetic-only","account_id":%q}}`, claims, account))
}
func TestInstallPreservesDocumentAndGuardsIdentity(t *testing.T) {
	home := t.TempDir()
	loggedOut := accounts.ActiveIdentity{Known: true}
	if err := Install(home, native("a"), loggedOut); err != nil {
		t.Fatal(err)
	}
	raw, a, err := Read(home)
	if err != nil || string(raw) != string(native("a")) {
		t.Fatalf("%v", err)
	}
	if err := Install(home, native("b"), loggedOut); err != ErrChanged {
		t.Fatalf("stale plan: %v", err)
	}
	if err := Install(home, native("b"), a); err != nil {
		t.Fatal(err)
	}
	raw, b, err := Read(home)
	if err != nil || b.AccountID != "b" || string(raw) != string(native("b")) {
		t.Fatal("wrong installed document")
	}
	if err := Clear(home, a); err != ErrChanged {
		t.Fatal("stale removal allowed")
	}
	if err := Clear(home, b); err != nil {
		t.Fatal(err)
	}
	_, empty, err := Read(home)
	if err != nil || !Equal(empty, loggedOut) {
		t.Fatalf("%+v %v", empty, err)
	}
}
func TestNativeFileSafety(t *testing.T) {
	for _, kind := range []string{"symlink", "public", "directory", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			name := filepath.Join(home, "auth.json")
			switch kind {
			case "symlink":
				other := filepath.Join(t.TempDir(), "auth.json")
				if err := os.WriteFile(other, native("a"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, name); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(name, native("a"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(name, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(name, 0700); err != nil {
					t.Fatal(err)
				}
			case "unsupported":
				if err := os.WriteFile(name, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, identity, err := Read(home); err == nil || identity.Known {
				t.Fatal("unsafe native file accepted")
			}
		})
	}
}
func TestPartialIdentityIsNotASelectionGuard(t *testing.T) {
	home := t.TempDir()
	if err := Install(home, native("a"), accounts.ActiveIdentity{Known: true, UserID: "u"}); err != ErrChanged {
		t.Fatal(err)
	}
}
