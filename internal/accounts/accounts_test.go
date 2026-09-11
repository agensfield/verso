package accounts

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseNativeAuth(t *testing.T) {
	raw := authFixture(t, "user-one", "workspace-one", "one@example.com", "access-one", map[string]any{
		"future_top_level": map[string]any{"opaque": "keep-me"},
	})
	auth, err := ParseNativeAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Email != "one@example.com" || auth.UserID != "user-one" || auth.AccountID != "workspace-one" {
		t.Fatalf("unexpected metadata: %#v", auth)
	}
	if formatted := fmt.Sprintf("%v %#v", auth, auth); strings.Contains(formatted, "keep-me") {
		t.Fatalf("formatted NativeAuth exposed credentials: %s", formatted)
	}

	var stored map[string]any
	if err := json.Unmarshal(auth.raw, &stored); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["future_top_level"]; !ok {
		t.Fatal("unknown top-level auth field was dropped")
	}
}

func TestParseNativeAuthRejectsUnsupportedAndUncertainIdentity(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want error
	}{
		{"api key only", []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"secret"}`), ErrAPIKeyAuth},
		{"missing user", authFixture(t, "", "workspace", "x@example.com", "access", nil), ErrNativeIdentity},
		{"missing account", authFixture(t, "user", "", "x@example.com", "access", nil), ErrNativeIdentity},
		{"non native mode", []byte(`{"auth_mode":"headers","tokens":{}}`), ErrUnsupportedAuth},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseNativeAuth(test.raw)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}

	conflict := authFixture(t, "user", "claim-account", "x@example.com", "access", map[string]any{
		"tokens_account_id": "stored-account",
	})
	if _, err := ParseNativeAuth(conflict); !errors.Is(err, ErrConflictingIdentity) {
		t.Fatalf("got %v, want conflicting identity", err)
	}
	if _, err := ParseNativeAuth([]byte(`{"tokens":{"access_token":"x","id_token":"not-a-jwt"}}`)); err == nil {
		t.Fatal("malformed JWT accepted")
	}
}

func TestStoreSaveListAndCredentials(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	auth := mustParse(t, authFixture(t, "user-one", "workspace-one", "one@example.com", "super-secret-access", map[string]any{
		"future_top_level": "opaque-secret-field",
	}))
	account, err := store.Save(auth, "")
	if err != nil {
		t.Fatal(err)
	}
	if !validUUID(account.ID) || account.Alias != account.Email {
		t.Fatalf("unexpected account: %#v", account)
	}

	assertMode(t, root, 0o700)
	assertMode(t, store.accountPath(account.ID), 0o600)
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0] != account {
		t.Fatalf("unexpected list: %#v", listed)
	}
	metadata, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"super-secret-access", "opaque-secret-field", "tokens", "credentials"} {
		if strings.Contains(string(metadata), secret) {
			t.Fatalf("metadata output exposed %q: %s", secret, metadata)
		}
	}

	credentials, err := store.Credentials(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(credentials), "super-secret-access") || !strings.Contains(string(credentials), "opaque-secret-field") {
		t.Fatalf("credential document did not retain opaque fields: %s", credentials)
	}
	credentials[0] = 'x'
	again, err := store.Credentials(account.ID)
	if err != nil || again[0] == 'x' {
		t.Fatal("Credentials did not return an independent copy")
	}
}

func TestSaveReenrollmentPreservesIDAndAlias(t *testing.T) {
	store := mustOpen(t)
	first := mustParse(t, authFixture(t, "user", "workspace", "old@example.com", "old-secret", nil))
	account, err := store.Save(first, "work")
	if err != nil {
		t.Fatal(err)
	}
	second := mustParse(t, authFixture(t, "user", "workspace", "new@example.com", "new-secret", nil))
	updated, err := store.Save(second, "ignored-new-alias")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != account.ID || updated.Alias != "work" || updated.Email != "new@example.com" {
		t.Fatalf("re-enrollment lost stable metadata: before=%#v after=%#v", account, updated)
	}
	credentials, err := store.Credentials(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(credentials), "old-secret") || !strings.Contains(string(credentials), "new-secret") {
		t.Fatalf("credentials were not replaced: %s", credentials)
	}
}

func TestEmailDoesNotDeduplicateAndLookupRejectsAmbiguity(t *testing.T) {
	store := mustOpen(t)
	first, err := store.Save(mustParse(t, authFixture(t, "user-one", "workspace", "same@example.com", "one", nil)), "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(mustParse(t, authFixture(t, "user-two", "workspace", "same@example.com", "two", nil)), "")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("distinct native identities were deduplicated by email")
	}
	if _, err := store.Find("same@example.com"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("got %v, want ambiguous lookup", err)
	}
	if found, err := store.Find(first.ID); err != nil || found != first {
		t.Fatalf("exact ID lookup failed: %#v, %v", found, err)
	}
	for _, query := range []string{"../" + first.ID, "subdir/account", ".."} {
		if _, err := store.Find(query); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("query %q: got %v, want unsafe path", query, err)
		}
	}
}

func TestUpdateCredentialsRequiresSameNativeIdentity(t *testing.T) {
	store := mustOpen(t)
	original, err := store.Save(mustParse(t, authFixture(t, "user", "workspace", "x@example.com", "old-secret", nil)), "x")
	if err != nil {
		t.Fatal(err)
	}
	wrong := mustParse(t, authFixture(t, "other-user", "workspace", "x@example.com", "wrong-secret", nil))
	if _, err := store.UpdateCredentials(original.ID, wrong); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("got %v, want identity mismatch", err)
	}
	credentials, err := store.Credentials(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(credentials), "old-secret") || strings.Contains(string(credentials), "wrong-secret") {
		t.Fatal("identity mismatch changed stored credentials")
	}
	tampered := mustParse(t, authFixture(t, "user", "workspace", "x@example.com", "tampered-secret", nil))
	tampered.UserID = "other-user"
	if _, err := store.UpdateCredentials(original.ID, tampered); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("got %v, want tampered parsed auth rejection", err)
	}
	matching := mustParse(t, authFixture(t, "user", "workspace", "new@example.com", "new-secret", nil))
	updated, err := store.UpdateCredentials("x", matching)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != original.ID || updated.Alias != original.Alias || updated.Email != "new@example.com" {
		t.Fatalf("unexpected updated metadata: %#v", updated)
	}
}

func TestRemoveRequiresAuthoritativeInactiveIdentity(t *testing.T) {
	store := mustOpen(t)
	account, err := store.Save(mustParse(t, authFixture(t, "user", "workspace", "x@example.com", "secret", nil)), "x")
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		active ActiveIdentity
		want   error
	}{
		{ActiveIdentity{}, ErrUnknownActive},
		{ActiveIdentity{Known: true, UserID: "user"}, ErrUnknownActive},
		{ActiveIdentity{Known: true, UserID: "user", AccountID: "workspace"}, ErrActiveAccount},
	}
	for _, check := range checks {
		if err := store.Remove(account.ID, check.active); !errors.Is(err, check.want) {
			t.Fatalf("got %v, want %v", err, check.want)
		}
		if _, err := store.Find(account.ID); err != nil {
			t.Fatalf("refused removal still deleted account: %v", err)
		}
	}
	if err := store.Remove(account.ID, ActiveIdentity{Known: true, UserID: "other", AccountID: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Find(account.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want removed account", err)
	}
}

func TestStoreRejectsSymlinksAndWrongSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Verso alpha targets macOS and Linux")
	}
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(linkRoot); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("got %v, want symlink root rejection", err)
	}

	store, err := Open(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	id := "123e4567-e89b-42d3-a456-426614174000"
	target := filepath.Join(base, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.accountPath(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("got %v, want symlink entry rejection", err)
	}
	if err := os.Remove(store.accountPath(id)); err != nil {
		t.Fatal(err)
	}

	bad := `{"schema_version":2,"account":{"id":"` + id + `","user_id":"u","account_id":"a"},"credentials":{}}`
	if err := os.WriteFile(store.accountPath(id), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("got %v, want schema rejection", err)
	}
}

func authFixture(t *testing.T, userID, accountID, email, accessToken string, options map[string]any) []byte {
	t.Helper()
	claims := map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    userID,
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	tokensAccountID := accountID
	if value, ok := options["tokens_account_id"].(string); ok {
		tokensAccountID = value
	}
	doc := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      token,
			"access_token":  accessToken,
			"refresh_token": "refresh-secret",
			"account_id":    tokensAccountID,
			"future_token":  "keep-token-field",
		},
		"last_refresh": "2026-09-11T00:00:00Z",
	}
	for key, value := range options {
		if key != "tokens_account_id" {
			doc[key] = value
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustParse(t *testing.T, raw []byte) NativeAuth {
	t.Helper()
	auth, err := ParseNativeAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func mustOpen(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
