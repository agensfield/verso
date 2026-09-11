package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/codex"
)

type fixtureResolver struct{}

func (fixtureResolver) ResolveCredentialConfig(context.Context, codex.CredentialResolveRequest) (codex.CredentialResolution, error) {
	return codex.CredentialResolution{EffectiveMode: "file", Basis: "fixture", Snapshot: "fixture"}, nil
}

type quotaClient struct {
	used      []string
	refreshes int
}

func (c *quotaClient) DeviceLogin(context.Context, func(auth.DevicePrompt) error) ([]byte, error) {
	panic("unexpected device flow")
}
func (c *quotaClient) Refresh(context.Context, []byte) ([]byte, error) {
	c.refreshes++
	return nil, auth.ErrLoginRequired
}
func (c *quotaClient) Usage(_ context.Context, raw []byte) (auth.Quota, error) {
	c.used = append(c.used, string(raw))
	return auth.Quota{}, nil
}
func quotaAuth(account, token string) []byte {
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":"same@example.test","https://api.openai.com/auth":{"chatgpt_user_id":"user","chatgpt_account_id":%q}}`, account)))
	return []byte(fmt.Sprintf(`{"tokens":{"id_token":"e30.%s.sig","access_token":%q,"account_id":%q}}`, claims, token, account))
}
func TestQuotaUsesAuthoritativeActiveFileWithoutRefresh(t *testing.T) {
	a, out, errOut := appFixture(t)
	a.CredentialResolver = fixtureResolver{}
	client := &quotaClient{}
	a.Auth = client
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := accounts.ParseNativeAuth(quotaAuth("a", "saved-old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(parsed, "personal"); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(a.CodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.CodexHome, "auth.json"), quotaAuth("a", "live-current"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.CodexHome, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := a.Run(context.Background(), []string{"quota", "personal", "--json"}); code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if client.refreshes != 0 || len(client.used) != 1 || !strings.Contains(client.used[0], "live-current") {
		t.Fatalf("wrong active quota path: %+v", client)
	}
	if strings.Contains(out.String(), "live-current") || strings.Contains(out.String(), "saved-old") {
		t.Fatal("credential leak")
	}
}
func TestUnknownModeSkipsCredentialRefreshDuringQuotaBrowse(t *testing.T) {
	a, _, _ := appFixture(t)
	client := &quotaClient{}
	a.Auth = client
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := accounts.ParseNativeAuth(quotaAuth("a", "saved-old"))
	if _, err = store.Save(parsed, "personal"); err != nil {
		t.Fatal(err)
	}
	if a.Run(context.Background(), []string{"quota"}) != 0 {
		t.Fatal("unknown quota should be displayable")
	}
	if client.refreshes != 0 || len(client.used) != 0 {
		t.Fatal("unknown selection reached network")
	}
}
func TestSwitchCommandRejectsAgentBeforeInspection(t *testing.T) {
	a, _, _ := appFixture(t)
	a.Env = []string{"CODEX_THREAD_ID=synthetic-thread"}
	a.RunCommand = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("agent switch reached runtime")
		return nil, nil
	}
	if a.Run(context.Background(), []string{"switch", "personal"}) == 0 {
		t.Fatal("agent switch accepted")
	}
	if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
		t.Fatal("agent switch wrote state")
	}
}

func TestImportAndRemovalPreserveNativeSelection(t *testing.T) {
	a, out, errOut := appFixture(t)
	a.CredentialResolver = fixtureResolver{}
	if err := os.MkdirAll(a.CodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	raw := quotaAuth("a", "native-current")
	name := filepath.Join(a.CodexHome, "auth.json")
	if err := os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.CodexHome, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if a.Run(context.Background(), []string{"import", "personal"}) != 0 {
		t.Fatal(errOut.String())
	}
	if strings.Contains(out.String(), "native-current") {
		t.Fatal("import leaked credentials")
	}
	if a.Run(context.Background(), []string{"remove", "personal"}) == 0 {
		t.Fatal("active removal succeeded")
	}
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	inactive, _ := accounts.ParseNativeAuth(quotaAuth("b", "saved-other"))
	if _, err = store.Save(inactive, "work"); err != nil {
		t.Fatal(err)
	}
	if a.Run(context.Background(), []string{"remove", "work"}) != 0 {
		t.Fatal(errOut.String())
	}
	after, _ := os.ReadFile(name)
	if string(after) != string(raw) {
		t.Fatal("account command changed native auth")
	}
}
