package accounts

import (
	"bytes"
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

func TestImplicitNativeModePrecedence(t *testing.T) {
	for _, key := range []string{"OPENAI_API_KEY", "personal_access_token", "bedrock_api_key", "bedrock_access_keys"} {
		raw := authFixture(t, "u", "a", "one@example.com", "access", nil)
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, "auth_mode")
		doc[key] = "synthetic-other-auth"
		encoded, _ := json.Marshal(doc)
		if _, err := ParseNativeAuth(encoded); err == nil {
			t.Fatalf("accepted implicit %s as ChatGPT", key)
		}
		doc["auth_mode"] = "chatgpt"
		encoded, _ = json.Marshal(doc)
		if _, err := ParseNativeAuth(encoded); err != nil {
			t.Fatalf("explicit chatgpt: %v", err)
		}
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
	mode := "untrusted-secret-auth-mode"
	_, err := ParseNativeAuth([]byte(`{"auth_mode":"` + mode + `","tokens":{}}`))
	if !errors.Is(err, ErrUnsupportedAuth) || strings.Contains(err.Error(), mode) {
		t.Fatalf("unsupported mode error exposed raw value: %v", err)
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

func TestSaveReenrollmentAppliesExplicitAlias(t *testing.T) {
	store := mustOpen(t)
	first := mustParse(t, authFixture(t, "user", "workspace", "old@example.com", "old-secret", nil))
	account, err := store.Save(first, "work")
	if err != nil {
		t.Fatal(err)
	}
	second := mustParse(t, authFixture(t, "user", "workspace", "new@example.com", "new-secret", nil))
	updated, err := store.Save(second, "new-alias")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != account.ID || updated.Alias != "new-alias" || updated.Email != "new@example.com" {
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

func TestSaveReenrollmentWithoutAliasPreservesAlias(t *testing.T) {
	store := mustOpen(t)
	first := mustParse(t, authFixture(t, "user", "workspace", "old@example.com", "old-secret", nil))
	account, err := store.Save(first, "work")
	if err != nil {
		t.Fatal(err)
	}
	second := mustParse(t, authFixture(t, "user", "workspace", "new@example.com", "new-secret", nil))
	updated, err := store.Save(second, "")
	if err != nil || updated.ID != account.ID || updated.Alias != "work" {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
}

func TestSaveReenrollmentRevalidatesAliasAgainstOtherAccounts(t *testing.T) {
	store := mustOpen(t)
	first, err := store.Save(mustParse(t, authFixture(t, "user-one", "workspace", "one@example.com", "old-secret", nil)), "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(mustParse(t, authFixture(t, "user-two", "workspace", "two@example.com", "other-secret", nil)), "second"); err != nil {
		t.Fatal(err)
	}
	reauth := mustParse(t, authFixture(t, "user-one", "workspace", "one@example.com", "new-secret", nil))
	if _, err := store.Save(reauth, "second"); !errors.Is(err, ErrAliasConflict) {
		t.Fatalf("collision error=%v", err)
	}
	raw, err := store.Credentials(first.ID)
	if err != nil || !strings.Contains(string(raw), "old-secret") || strings.Contains(string(raw), "new-secret") {
		t.Fatal("rejected alias collision changed credentials")
	}
	updated, err := store.Save(reauth, "first")
	if err != nil || updated.ID != first.ID || updated.Alias != "first" {
		t.Fatalf("same-account alias rejected: updated=%#v err=%v", updated, err)
	}
}

func TestRenameValidatesCollisionsAndPreservesCredentials(t *testing.T) {
	store := mustOpen(t)
	first, err := store.Save(mustParse(t, authFixture(t, "user-one", "workspace", "one@example.com", "secret-one", nil)), "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(mustParse(t, authFixture(t, "user-two", "workspace", "two@example.com", "secret-two", nil)), "second")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Credentials(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"second", second.Email} {
		if _, err := store.Rename(first.ID, alias); !errors.Is(err, ErrAliasConflict) {
			t.Fatalf("alias %q: err=%v", alias, err)
		}
	}
	renamed, err := store.Rename(first.ID, first.Email)
	if err != nil || renamed.Alias != first.Email || renamed.ID != first.ID {
		t.Fatalf("renamed=%#v err=%v", renamed, err)
	}
	after, err := store.Credentials(first.ID)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("Rename changed stored credential bytes")
	}
}

func TestValidateAliasRejectsUnsafeSyntax(t *testing.T) {
	if err := ValidateAlias(""); err != nil {
		t.Fatalf("empty optional alias: %v", err)
	}
	for _, alias := range []string{" spaced ", "../escape", "123e4567-e89b-42d3-a456-426614174000"} {
		if !errors.Is(ValidateAlias(alias), ErrInvalidAlias) {
			t.Fatalf("alias %q accepted", alias)
		}
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

func TestSaveRejectsLookupCollidingExplicitAliases(t *testing.T) {
	store := mustOpen(t)
	first, err := store.Save(mustParse(t, authFixture(t, "user-one", "workspace", "one@example.com", "one", nil)), "first")
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{first.ID, "first", "one@example.com"} {
		_, err := store.Save(mustParse(t, authFixture(t, "user-"+alias, "workspace", "two@example.com", "two", nil)), alias)
		want := ErrAliasConflict
		if alias == first.ID {
			want = ErrInvalidAlias
		}
		if !errors.Is(err, want) {
			t.Fatalf("alias %q: got %v, want %v", alias, err, want)
		}
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("rejected aliases created accounts: %#v", listed)
	}
}

func TestOpenReadOnlyDoesNotMutateFilesystem(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing", "accounts")
	store, err := OpenReadOnly(missing)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 0 {
		t.Fatalf("missing read-only store list = %#v, %v", listed, err)
	}
	if _, err := os.Lstat(filepath.Join(base, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created filesystem state: %v", err)
	}
	auth := mustParse(t, authFixture(t, "user", "workspace", "x@example.com", "secret", nil))
	if _, err := store.Save(auth, "x"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Save returned %v", err)
	}
	if _, err := store.UpdateCredentials("x", auth); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only UpdateCredentials returned %v", err)
	}
	if _, err := store.Rename("x", "renamed"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Rename returned %v", err)
	}
	if err := store.Remove("x", ActiveIdentity{Known: true}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Remove returned %v", err)
	}

	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(existing); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Fatalf("read-only open changed mode from %v to %v", before.Mode(), after.Mode())
	}
	if err := os.Chmod(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(existing); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("insecure read-only root returned %v", err)
	}
	assertMode(t, existing, 0o755)
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

func TestListPartialReturnsHealthyAccountsAndSanitizedIssues(t *testing.T) {
	store := mustOpen(t)
	healthy, err := store.Save(mustParse(t, authFixture(t, "user", "workspace", "healthy@example.com", "healthy-secret", nil)), "healthy")
	if err != nil {
		t.Fatal(err)
	}
	badID := "123e4567-e89b-42d3-a456-426614174000"
	bad := `{"schema_version":999,"secret":"must-not-leak"}`
	if err := os.WriteFile(store.accountPath(badID), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "private-name.json"), []byte("must-not-leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	listed, issues, err := store.ListPartial()
	if err != nil || len(listed) != 1 || listed[0] != healthy || len(issues) != 2 {
		t.Fatalf("listed=%#v issues=%#v err=%v", listed, issues, err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("strict List accepted malformed inventory")
	}
	if found, err := store.Find(healthy.ID); err != nil || found != healthy {
		t.Fatalf("exact healthy lookup failed: found=%#v err=%v", found, err)
	}
	encoded, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{store.root, "private-name", "must-not-leak", "secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("issues exposed %q: %s", forbidden, encoded)
		}
	}
	if issues[0].Code == "" || issues[0].Message == "" || issues[1].Code == "" || issues[1].Message == "" {
		t.Fatalf("issues lack explicit classifications: %#v", issues)
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
