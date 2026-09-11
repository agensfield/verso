package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/switcher"
)

func appFixture(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	home := t.TempDir()
	a := &App{StateDir: filepath.Join(home, "state"), CodexHome: filepath.Join(home, "codex"), Version: "test", Env: []string{}, Out: out, Err: errOut, RunCommand: func(context.Context, string, ...string) ([]byte, error) { return nil, nil }}
	return a, out, errOut
}
func TestHelpAndVersionDoNotTouchState(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"version"}, {"--version"}} {
		a, out, _ := appFixture(t)
		if code := a.Run(context.Background(), args); code != 0 {
			t.Fatal(code)
		}
		if out.Len() == 0 {
			t.Fatal("empty output")
		}
		if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
			t.Fatal("created state")
		}
	}
}
func TestListIsReadOnlyAndJSONFlagsInterspersed(t *testing.T) {
	a, out, _ := appFixture(t)
	if code := a.Run(context.Background(), []string{"list", "--json"}); code != 0 {
		t.Fatal(code)
	}
	var result response
	if json.Unmarshal(out.Bytes(), &result) != nil || !result.OK || result.Schema != "verso/v1" {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
		t.Fatal("list created store")
	}
}
func TestListAndPreviewDoNotExposeCredentials(t *testing.T) {
	a, out, _ := appFixture(t)
	a.CredentialResolver = fixtureResolver{}
	s, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"a@example.test","https://api.openai.com/auth":{"chatgpt_user_id":"u","chatgpt_account_id":"a"}}`))
	raw := []byte(`{"tokens":{"id_token":"e30.` + claims + `.sig","access_token":"NEVER-PRINT","account_id":"a"}}`)
	native, err := accounts.ParseNativeAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(native, "personal"); err != nil {
		t.Fatal(err)
	}
	if code := a.Run(context.Background(), []string{"list", "--json"}); code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(out.String(), "NEVER-PRINT") || strings.Contains(out.String(), claims) {
		t.Fatal("credential leak")
	}
	out.Reset()
	if err := os.MkdirAll(a.CodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.CodexHome, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := a.Run(context.Background(), []string{"preview", "personal", "--json"}); code != 0 {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), "NEVER-PRINT") {
		t.Fatal("preview leak")
	}
	if _, err := os.Stat(filepath.Join(a.CodexHome, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("preview activated account")
	}
}
func TestInvalidCommandAndFlagsFail(t *testing.T) {
	for _, args := range [][]string{{"bogus"}, {"list", "extra"}, {"preview"}, {"list", "--bogus"}} {
		a, _, _ := appFixture(t)
		if code := a.Run(context.Background(), args); code == 0 {
			t.Fatal(args)
		}
	}
}

func TestRecoveryIsReadOnlyAndReportsUnfinishedOperation(t *testing.T) {
	a, out, _ := appFixture(t)
	if a.Run(context.Background(), []string{"recovery", "--json"}) != 0 {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
		t.Fatal("recovery created state")
	}
	if err := os.MkdirAll(a.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Use the actual journal DTO so recovery checks schema rather than fixture spelling.
	cp := switcher.Checkpoint{Version: 1, From: "old", Target: "new", HadDaemon: true, Phase: "starting"}
	raw, _ := json.Marshal(cp)
	name := filepath.Join(a.StateDir, "switch.json")
	if err := os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if a.Run(context.Background(), []string{"recovery", "--json"}) != 0 {
		t.Fatal(out.String())
	}
	var r response
	if json.Unmarshal(out.Bytes(), &r) != nil || r.Journal == nil || r.Journal.Phase != "starting" {
		t.Fatal(out.String())
	}
	after, _ := os.ReadFile(name)
	if string(after) != string(raw) {
		t.Fatal("recovery changed journal")
	}
}
