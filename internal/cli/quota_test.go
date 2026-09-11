package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	used        []string
	refreshes   int
	refreshed   []byte
	usageErrors []error
	onUsage     func()
}

func (c *quotaClient) DeviceLogin(context.Context, func(auth.DevicePrompt) error) ([]byte, error) {
	panic("unexpected device flow")
}
func (c *quotaClient) Refresh(context.Context, []byte) ([]byte, error) {
	c.refreshes++
	if c.refreshed == nil {
		return nil, auth.ErrLoginRequired
	}
	return c.refreshed, nil
}
func (c *quotaClient) Usage(_ context.Context, raw []byte) (auth.Quota, error) {
	c.used = append(c.used, string(raw))
	if c.onUsage != nil {
		c.onUsage()
	}
	if len(c.usageErrors) > 0 {
		err := c.usageErrors[0]
		c.usageErrors = c.usageErrors[1:]
		return auth.Quota{}, err
	}
	used := 23.0
	return auth.Quota{Primary: &auth.Window{UsedPercent: &used}}, nil
}
func quotaAuth(account, token string) []byte {
	return quotaAuthEmail(account, token, "same@example.test")
}

func quotaAuthEmail(account, token, email string) []byte {
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":%q,"https://api.openai.com/auth":{"chatgpt_user_id":"user","chatgpt_account_id":%q}}`, email, account)))
	access := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`))
	return []byte(fmt.Sprintf(`{"marker":%q,"tokens":{"id_token":"e30.%s.sig","access_token":"e30.%s.sig","refresh_token":"synthetic-only","account_id":%q}}`, token, claims, access, account))
}
func TestListUsesAuthoritativeActiveFileWithoutRefresh(t *testing.T) {
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
	if code := a.Run(context.Background(), []string{"list", "personal", "--json"}); code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if client.refreshes != 0 || len(client.used) != 1 || !strings.Contains(client.used[0], "live-current") {
		t.Fatalf("wrong active list path: %+v", client)
	}
	if strings.Contains(out.String(), "live-current") || strings.Contains(out.String(), "saved-old") {
		t.Fatal("credential leak")
	}
}

func TestListRejectsActiveSelectionChangeDuringUsage(t *testing.T) {
	a, out, _ := appFixture(t)
	a.CredentialResolver = fixtureResolver{}
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := accounts.ParseNativeAuth(quotaAuth("a", "saved"))
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.Save(parsed, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(a.CodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(a.CodexHome, "auth.json")
	if err = os.WriteFile(authPath, quotaAuth("a", "live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.CodexHome, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &quotaClient{onUsage: func() {
		if err := os.WriteFile(authPath, quotaAuth("other", "changed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	a.Auth = client
	if code := a.Run(context.Background(), []string{"list", "--json"}); code == 0 {
		t.Fatal("selection change was accepted")
	}
	if client.refreshes != 0 || len(client.used) != 1 {
		t.Fatalf("unexpected active operations: %+v", client)
	}
	if _, err = os.Stat(filepath.Join(a.StateDir, "quota.json")); !os.IsNotExist(err) {
		t.Fatalf("selection change wrote quota cache: %v", err)
	}
	stored, err := store.Credentials(account.ID)
	if err != nil || !strings.Contains(string(stored), "saved") {
		t.Fatal("selection change touched saved credentials")
	}
	if strings.Contains(out.String(), "marker") || strings.Contains(out.String(), "synthetic-only") {
		t.Fatalf("credential leak: %s", out)
	}
}
func TestUnknownModeSkipsCredentialRefreshDuringList(t *testing.T) {
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
	if a.Run(context.Background(), []string{"list", "--json"}) != 0 {
		t.Fatal("unknown usage should be displayable")
	}
	if client.refreshes != 0 || len(client.used) != 0 {
		t.Fatal("unknown selection reached network")
	}
}

func TestDefaultListRefreshesInactiveCredentialsAndWritesQuota(t *testing.T) {
	a, out, errOut := appFixture(t)
	a.CredentialResolver = fixtureResolver{}
	client := &quotaClient{refreshed: quotaAuthEmail("a", "refreshed", "new@example.test")}
	a.Auth = client
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := accounts.ParseNativeAuth(quotaAuth("a", "saved-old"))
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.Save(parsed, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(a.CodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.CodexHome, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.usageErrors = []error{auth.ErrLoginRequired}
	if code := a.Run(context.Background(), []string{"list", "--json"}); code != 0 {
		t.Fatalf("%d: %s / %s", code, out, errOut)
	}
	if client.refreshes != 1 || len(client.used) != 2 {
		t.Fatalf("expected usage retry with one inactive refresh: %+v", client)
	}
	stored, err := store.Credentials(account.ID)
	if err != nil || !strings.Contains(string(stored), "refreshed") {
		t.Fatalf("refreshed credentials not stored: %v", err)
	}
	if _, err = os.Stat(filepath.Join(a.StateDir, "quota.json")); err != nil {
		t.Fatalf("quota cache not written: %v", err)
	}
	var result response
	if err = json.Unmarshal(out.Bytes(), &result); err != nil || result.Cached || result.Quotas[account.ID].Quota == nil {
		t.Fatalf("unexpected list response: %+v / %v", result, err)
	}
	if len(result.Accounts) != 1 || result.Accounts[0].Email != "new@example.test" {
		t.Fatalf("refreshed account metadata not rendered: %+v", result.Accounts)
	}
}

func TestCachedListUsesNoNetworkRPCOrWrites(t *testing.T) {
	a, out, errOut := appFixture(t)
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := accounts.ParseNativeAuth(quotaAuth("a", "saved"))
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.Save(parsed, "personal")
	if err != nil {
		t.Fatal(err)
	}
	client := &quotaClient{}
	a.Auth = client
	a.RunCommand = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("cached list probed Codex")
		return nil, nil
	}
	before, err := os.ReadFile(filepath.Join(a.StateDir, "accounts", account.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if code := a.Run(context.Background(), []string{"list", "--cached", "--json"}); code != 0 {
		t.Fatalf("%d: %s / %s", code, out, errOut)
	}
	after, err := os.ReadFile(filepath.Join(a.StateDir, "accounts", account.ID+".json"))
	if err != nil || string(after) != string(before) {
		t.Fatal("cached list changed account data")
	}
	if client.refreshes != 0 || len(client.used) != 0 {
		t.Fatalf("cached list reached network: %+v", client)
	}
	if _, err = os.Stat(filepath.Join(a.StateDir, "quota.json")); !os.IsNotExist(err) {
		t.Fatalf("cached list created quota cache: %v", err)
	}
	var result response
	if err = json.Unmarshal(out.Bytes(), &result); err != nil || !result.Cached {
		t.Fatalf("unexpected cached response: %+v / %v", result, err)
	}
}

func TestQuotaCommandWasRemoved(t *testing.T) {
	a, _, _ := appFixture(t)
	if a.Run(context.Background(), []string{"quota"}) == 0 {
		t.Fatal("removed quota command still dispatched")
	}
	if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
		t.Fatal("removed command touched state")
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
