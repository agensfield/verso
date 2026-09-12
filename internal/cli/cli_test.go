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
	"github.com/agensfield/verso/internal/codex"
	"github.com/agensfield/verso/internal/switcher"
	"github.com/agensfield/verso/internal/updater"
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

func TestHelpIsCompactAndRoutesCommandDetails(t *testing.T) {
	a, out, _ := appFixture(t)
	if code := a.Run(context.Background(), []string{"--help"}); code != 0 {
		t.Fatal(code)
	}
	compact := out.String()
	if !strings.Contains(compact, "Usage: verso <command>") || !strings.Contains(compact, "verso --skill") {
		t.Fatal(compact)
	}
	if lines := strings.Count(strings.TrimSpace(compact), "\n") + 1; lines > 14 {
		t.Fatalf("top-level help grew to %d lines:\n%s", lines, compact)
	}
	if strings.Contains(compact, "--allow-exhausted") || strings.Contains(compact, "--state-dir") {
		t.Fatal("advanced flags leaked into compact help")
	}
	if strings.Contains(compact, "\x1b[") {
		t.Fatal("piped help contains ANSI")
	}
	out.Reset()
	if code := a.Run(context.Background(), []string{"help", "list"}); code != 0 {
		t.Fatal(code)
	}
	if detail := out.String(); !strings.Contains(detail, "--cached") || !strings.Contains(detail, "no network, RPC, or writes") {
		t.Fatal(detail)
	}
	out.Reset()
	if code := a.Run(context.Background(), []string{"list", "--help"}); code != 0 || !strings.Contains(out.String(), "--cached") {
		t.Fatalf("command --help did not route: %d %s", code, out)
	}
}

func TestHumanStatusAndTargetUseOperatorLabels(t *testing.T) {
	a, out, _ := appFixture(t)
	account := accounts.Account{ID: "hidden-id", Alias: "personal", Email: "same@example.test", UserID: "user", AccountID: "workspace"}
	runtime := &codex.Observation{
		Daemon:        switcher.Running,
		Config:        codex.Config{CredentialStore: "file"},
		SelectedFile:  accounts.ActiveIdentity{Known: true, UserID: "user", AccountID: "workspace"},
		SelectedEmail: "same@example.test",
		Busy:          []string{"hidden-turn-1", "hidden-turn-2"},
	}
	plan := &switcher.Plan{Inspection: switcher.Inspection{Daemon: switcher.Running, Busy: []string{"hidden-plan-turn"}}}
	if code := a.finish(response{Command: "preview", Accounts: []accounts.Account{account}, Runtime: runtime, Plan: plan, Target: &account}, nil); code != 0 {
		t.Fatal(code)
	}
	got := out.String()
	for _, want := range []string{"Codex: running", "Credentials: file", "Account: personal", "Busy conversations: 1", "Busy conversations: 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	for _, hidden := range []string{"Target:", `"personal"`, "same@example.test", "hidden-id", "hidden-turn", "Selected file identity", "Credential mode"} {
		if strings.Contains(got, hidden) {
			t.Errorf("human output contains %q:\n%s", hidden, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatal("piped status contains ANSI")
	}
}
func TestEmptyListCreatesNothingAndJSONFlagsAreInterspersed(t *testing.T) {
	for _, args := range [][]string{{"list", "--json"}, {"list", "--cached", "--json"}} {
		a, out, _ := appFixture(t)
		a.RunCommand = func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("empty list probed Codex")
			return nil, nil
		}
		if code := a.Run(context.Background(), args); code != 0 {
			t.Fatal(code)
		}
		var result response
		if json.Unmarshal(out.Bytes(), &result) != nil || !result.OK || result.Schema != "verso/v1" {
			t.Fatal(out.String())
		}
		if strings.Contains(strings.Join(args, " "), "--cached") != result.Cached {
			t.Fatalf("cached mode not represented: %+v", result)
		}
		if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
			t.Fatal("empty list created state")
		}
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
	client := &quotaClient{}
	a.Auth = client
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
	if client.refreshes != 0 || len(client.used) != 0 {
		t.Fatalf("preview reached network: %+v", client)
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

func TestCommandFlagsAreRejectedBeforeEffects(t *testing.T) {
	for _, args := range [][]string{
		{"remove", "missing", "--check=false"},
		{"import", "work", "--cached=false"},
		{"add", "work", "--check"},
		{"update", "--cached"},
		{"status", "--allow-no-snapshot=false"},
	} {
		a, out, _ := appFixture(t)
		a.UpdateAction = func(context.Context, bool) (updater.Result, error) {
			t.Fatal("invalid flag dispatched update")
			return updater.Result{}, nil
		}
		if code := a.Run(context.Background(), append(args, "--json")); code == 0 {
			t.Fatalf("accepted flags: %v", args)
		}
		var result response
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.ErrorCode != "invalid_arguments" {
			t.Fatalf("unexpected result for %v: %s", args, out.String())
		}
		if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
			t.Fatalf("invalid invocation touched state: %v", args)
		}
	}
}

func TestJSONIntentSurvivesParseAndHelp(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "--bogus"},
		{"list", "--cached=nah", "--json"},
		{"--json=false", "list", "--cached=nah", "--json"},
		{"--json", "--state-dir"},
		{"--json"},
		{"help", "list", "--json"},
		{"list", "--help", "--json"},
	} {
		a, out, errOut := appFixture(t)
		a.Run(context.Background(), args)
		var result response
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Errorf("%v did not return JSON: stdout=%q stderr=%q", args, out.String(), errOut.String())
		}
		if errOut.Len() != 0 {
			t.Errorf("%v wrote JSON error to stderr: %q", args, errOut.String())
		}
	}

	a, out, _ := appFixture(t)
	a.Run(context.Background(), []string{"--json", "list", "--cached=nah", "--json=false"})
	if out.Len() != 0 {
		t.Fatalf("final --json=false did not select human output: %q", out.String())
	}

	// A token consumed as a string flag value is not output intent.
	a, _, errOut := appFixture(t)
	a.Run(context.Background(), []string{"--state-dir", "--json", "--bogus"})
	if !strings.Contains(errOut.String(), "verso:") {
		t.Fatalf("consumed --json incorrectly enabled JSON: %q", errOut.String())
	}
}

func TestUnknownCommandDiagnosticsDoNotEchoTerminalControls(t *testing.T) {
	a, _, errOut := appFixture(t)
	command := "unknown\x1b[2J"
	if code := a.Run(context.Background(), []string{command, "--check"}); code == 0 {
		t.Fatal("unknown command succeeded")
	}
	if strings.Contains(errOut.String(), command) || strings.Contains(errOut.String(), "\x1b") {
		t.Fatalf("unsafe command reached diagnostic: %q", errOut.String())
	}
}

func TestShortcutFlagErrorsAreInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"--skill", "--check=false", "--json"}, {"--version", "--cached=false", "--json"}} {
		a, out, _ := appFixture(t)
		if code := a.Run(context.Background(), args); code == 0 {
			t.Fatalf("accepted %v", args)
		}
		var result response
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.ErrorCode != "invalid_arguments" {
			t.Fatalf("%v: %s", args, out.String())
		}
	}
}

func TestOfflineSchemaAndTypedVersionMetadata(t *testing.T) {
	for _, args := range [][]string{{"schema", "--json"}, {"version", "--json"}, {"--version", "--json"}, {"--skill", "--json"}} {
		a, out, _ := appFixture(t)
		if code := a.Run(context.Background(), args); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, out.String())
		}
		var result response
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK {
			t.Fatalf("%v: %s", args, out.String())
		}
		if result.Command == "version" && (result.VersionInfo == nil || result.VersionInfo.Version != "test") {
			t.Fatalf("missing version metadata: %s", out.String())
		}
		if (result.Command == "schema" || result.Command == "skill") && (result.Contract == nil || len(result.Contract.Commands) == 0) {
			t.Fatalf("missing contract metadata: %s", out.String())
		}
		if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
			t.Fatalf("offline command touched state: %v", args)
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
