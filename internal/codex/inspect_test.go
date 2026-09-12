package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agensfield/verso/internal/switcher"
	"github.com/coder/websocket"
)

func testNativeAuth(t *testing.T, home string) {
	t.Helper()
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"a@example.test","https://api.openai.com/auth":{"chatgpt_user_id":"user-a","chatgpt_account_id":"account-a"}}`))
	raw := `{"tokens":{"id_token":"e30.` + claims + `.sig","access_token":"synthetic-only","account_id":"account-a"}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}
func noProcesses(context.Context, string, ...string) ([]byte, error) { return []byte(""), nil }
func TestConfirmedAbsenceAndStandaloneWarnings(t *testing.T) {
	home := t.TempDir()
	testNativeAuth(t, home)
	i := Inspector{Home: home, Run: noProcesses}
	o, err := i.Inspect(context.Background())
	if err != nil || o.Daemon != switcher.Stopped || o.SelectedFile.AccountID != "account-a" {
		t.Fatalf("%+v %v", o, err)
	}
	i.Run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("1234 /bin/codex resume saved-thread\n"), nil
	}
	o, err = i.Inspect(context.Background())
	if err != nil || o.Daemon != switcher.Stopped || len(o.Warnings) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}
func TestAbsenceCannotBeInferredFromUnreachableOrUnmanaged(t *testing.T) {
	home := t.TempDir()
	testNativeAuth(t, home)
	i := Inspector{Home: home, Run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("permission denied") }}
	if o, err := i.Inspect(context.Background()); err == nil || o.Daemon != switcher.Unknown {
		t.Fatalf("%+v %v", o, err)
	}
	i.Run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("1234 /bin/codex app-server --listen unix://\n"), nil
	}
	if o, err := i.Inspect(context.Background()); err == nil || o.Daemon != switcher.Unknown {
		t.Fatalf("%+v %v", o, err)
	}
	i.Run = noProcesses
	if err := os.MkdirAll(filepath.Dir(i.Socket()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.Socket(), []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	if o, err := i.Inspect(context.Background()); err == nil || o.Daemon != switcher.Unknown {
		t.Fatalf("%+v %v", o, err)
	}
}
func TestAlternativeAuthPresenceRefusesWithoutEchoingValue(t *testing.T) {
	home := t.TempDir()
	testNativeAuth(t, home)
	i := Inspector{Home: home, Run: noProcesses, Env: []string{"CODEX_ACCESS_TOKEN=do-not-echo"}}
	o, err := i.Inspect(context.Background())
	if err != nil || o.Daemon != switcher.Stopped || o.Credential.Status != CredentialUnknown || strings.Contains(o.Credential.Reason, "do-not-echo") {
		t.Fatalf("%+v %v", o, err)
	}
}

type fixedResolver struct {
	result CredentialResolution
	err    error
}

func (r fixedResolver) ResolveCredentialConfig(context.Context, CredentialResolveRequest) (CredentialResolution, error) {
	return r.result, r.err
}

func TestStoppedCredentialProofRequiresCompleteResolverAndCWD(t *testing.T) {
	home := t.TempDir()
	testNativeAuth(t, home)
	i := Inspector{Home: home, Run: noProcesses}
	o, err := i.Inspect(context.Background())
	if err != nil || o.Daemon != switcher.Stopped || o.Credential.Status != CredentialUnknown {
		t.Fatalf("%+v %v", o, err)
	}
	i.CWD = t.TempDir()
	i.LaunchEnvKnown = true
	i.Resolver = fixedResolver{result: CredentialResolution{EffectiveMode: "file", Basis: "synthetic-complete-stack", Snapshot: "snapshot"}}
	o, err = i.Inspect(context.Background())
	if err != nil || o.Credential.Status != CredentialFileSelected || o.Credential.FileIdentity.AccountID != "account-a" {
		t.Fatalf("%+v %v", o, err)
	}
	i.Resolver = fixedResolver{err: resolutionError("project config requires full Codex resolution")}
	o, err = i.Inspect(context.Background())
	if err != nil || o.Credential.Reason != "project config requires full Codex resolution" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestStoppedInspectionExposesLocalModeAndMandatoryWarning(t *testing.T) {
	home := t.TempDir()
	testNativeAuth(t, home)
	i := Inspector{
		Home:           home,
		CWD:            t.TempDir(),
		Run:            noProcesses,
		LaunchEnvKnown: true,
		Resolver: fixedResolver{result: CredentialResolution{
			Status:        CredentialLocalFile,
			EffectiveMode: "file",
			Basis:         "explicit-local-file-mode-no-known-local-overrides",
			Snapshot:      "snapshot",
			Warning:       "enterprise cloud configuration remains unresolved until Codex is reopened",
		}},
	}
	o, err := i.Inspect(context.Background())
	if err != nil || o.Daemon != switcher.Stopped || o.Credential.Status != CredentialLocalFile || o.Credential.Warning == "" {
		t.Fatalf("%+v %v", o, err)
	}
	i.Resolver = fixedResolver{result: CredentialResolution{Status: CredentialLocalFile, EffectiveMode: "file", Basis: "local", Snapshot: "snapshot"}}
	o, err = i.Inspect(context.Background())
	if err != nil || o.Credential.Status != CredentialUnknown {
		t.Fatalf("missing warning must fail closed: %+v %v", o, err)
	}
}

func managedFixture(t *testing.T, status string) (Inspector, func()) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "verso-inspect-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	testNativeAuth(t, home)
	i := Inspector{Home: home, Version: "test"}
	pid := os.Getpid()
	birth := "Fri Sep 11 10:00:00 2026"
	if err := os.MkdirAll(filepath.Join(home, "app-server-daemon"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ProcessRecord{PID: pid, StartTime: birth})
	if err := os.WriteFile(filepath.Join(home, "app-server-daemon", "app-server.pid"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	i.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "/usr/sbin/lsof" {
			for _, arg := range args {
				if arg == "cwd" {
					return []byte("p" + strconv.Itoa(pid) + "\nn" + home + "\n"), nil
				}
			}
			return []byte("p" + strconv.Itoa(pid) + "\nn" + i.Socket() + "\n"), nil
		}
		for _, arg := range args {
			if arg == "lstart=" {
				return []byte(birth), nil
			}
		}
		if name == "readlink" {
			return []byte(home + "\n"), nil
		}
		return []byte(strconv.Itoa(pid) + " /bin/codex app-server --listen unix://\n"), nil
	}
	if err := os.MkdirAll(filepath.Dir(i.Socket()), 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", i.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(i.Socket(), 0600); err != nil {
		t.Fatal(err)
	}
	s := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, raw, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(raw, &req) != nil {
				return
			}
			var reply any
			switch req.Method {
			case "initialize":
				reply = ServerInfo{UserAgent: "codex_app_server/0.154.0", CodexHome: home, PlatformOS: "linux"}
			case "initialized":
				continue
			case "config/read":
				var params struct {
					IncludeLayers bool   `json:"includeLayers"`
					CWD           string `json:"cwd"`
				}
				if json.Unmarshal(req.Params, &params) != nil || !params.IncludeLayers || params.CWD != home {
					return
				}
				layers := []any{map[string]any{"name": map[string]any{"type": "user", "file": filepath.Join(home, "config.toml"), "profile": nil}, "version": "user-v1", "config": map[string]any{"cli_auth_credentials_store": "file"}}}
				if status == "noLayers" {
					layers = nil
				}
				reply = map[string]any{"config": Config{CredentialStore: "file"}, "layers": layers}
			case "configRequirements/read":
				if string(req.Params) != "null" {
					t.Errorf("configRequirements/read params = %s", req.Params)
					return
				}
				reply = map[string]any{"requirements": map[string]any{"cliAuthCredentialsStore": "file"}}
			case "account/read", "getAuthStatus":
				t.Errorf("unsafe identity RPC called: %s", req.Method)
				return
			case "thread/loaded/list":
				reply = map[string]any{"data": []string{"thread-a"}, "nextCursor": nil}
			case "thread/read":
				threadStatus := status
				if status == "noLayers" {
					threadStatus = "idle"
				}
				reply = map[string]any{"thread": map[string]any{"status": map[string]any{"type": threadStatus, "activeFlags": []string{"waitingOnApproval"}}}}
			default:
				reply = map[string]any{}
			}
			sendRPC(r.Context(), c, req.ID, reply)
		}
	})}
	go func() { _ = s.Serve(l) }()
	cleanup := func() { _ = s.Close(); _ = l.Close() }
	t.Cleanup(cleanup)
	return i, cleanup
}
func TestManagedRPCBusyAndIdle(t *testing.T) {
	for _, status := range []string{"idle", "active", "systemError"} {
		t.Run(status, func(t *testing.T) {
			i, _ := managedFixture(t, status)
			o, err := i.Inspect(context.Background())
			if status == "systemError" {
				if err == nil || o.Daemon != switcher.Running || o.ActivityKnown || o.ActivityError == "" {
					t.Fatalf("%+v %v", o, err)
				}
				return
			}
			if err != nil || o.Daemon != switcher.Running {
				t.Fatalf("%+v %v", o, err)
			}
			if o.Credential.Status != CredentialFileSelected {
				t.Fatalf("credential=%+v", o.Credential)
			}
			if !o.ActivityKnown || o.ActivityError != "" {
				t.Fatalf("activity=%t error=%q", o.ActivityKnown, o.ActivityError)
			}
			if (status == "active") != (len(o.Busy) == 1) {
				t.Fatalf("busy=%v", o.Busy)
			}
		})
	}
}

func TestMissingLayerEvidenceDoesNotHideRunningState(t *testing.T) {
	i, _ := managedFixture(t, "noLayers")
	o, err := i.Inspect(context.Background())
	if err != nil || o.Daemon != switcher.Running || o.Credential.Status != CredentialUnknown {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestFreshSelectionRequiresNativeWorkspaceIDsAndNewProcess(t *testing.T) {
	i, _ := managedFixture(t, "idle")
	i.LaunchEnvKnown = true
	expected, selectionErr := ReadNativeSelection(i.Home)
	if selectionErr != nil {
		t.Fatal(selectionErr)
	}
	proof, err := i.VerifyFreshSelection(context.Background(), ProcessRecord{PID: 999999, StartTime: "old birth"}, expected)
	if err != nil || proof.Status != CredentialFreshProcess {
		t.Fatalf("%+v %v", proof, err)
	}
	wrongWorkspace := expected
	wrongWorkspace.Identity.AccountID = "account-b"
	proof, err = i.VerifyFreshSelection(context.Background(), ProcessRecord{PID: 999999, StartTime: "old birth"}, wrongWorkspace)
	if err == nil || proof.Status != CredentialUnknown {
		t.Fatalf("%+v %v", proof, err)
	}
	o, inspectErr := i.Inspect(context.Background())
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	proof, err = i.VerifyFreshSelection(context.Background(), *o.Record, expected)
	if err == nil || proof.Status != CredentialUnknown {
		t.Fatalf("%+v %v", proof, err)
	}
}

func TestFreshSelectionSupportsLoggedOutRollback(t *testing.T) {
	i, _ := managedFixture(t, "idle")
	i.LaunchEnvKnown = true
	if err := os.Remove(filepath.Join(i.Home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	expected, err := ReadNativeSelection(i.Home)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := i.VerifyFreshSelection(context.Background(), ProcessRecord{PID: 999999, StartTime: "old birth"}, expected)
	if err != nil || proof.Status != CredentialFreshProcess || proof.FileIdentity.UserID != "" || !proof.FileIdentity.Known {
		t.Fatalf("%+v %v", proof, err)
	}
}

func TestFreshSelectionFailsClosedWhenOldBirthCannotBeRead(t *testing.T) {
	i, _ := managedFixture(t, "idle")
	i.LaunchEnvKnown = true
	expected, err := ReadNativeSelection(i.Home)
	if err != nil {
		t.Fatal(err)
	}
	run := i.Run
	i.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ps" && len(args) >= 2 && args[0] == "-p" && args[1] == "4242" {
			return nil, errors.New("gone or unreadable")
		}
		if name == "ps" && len(args) > 1 && args[0] == "-u" {
			current, callErr := run(ctx, name, args...)
			return append(current, []byte("4242 /bin/codex app-server\n")...), callErr
		}
		return run(ctx, name, args...)
	}
	proof, err := i.VerifyFreshSelection(context.Background(), ProcessRecord{PID: 4242, StartTime: "old birth"}, expected)
	if err == nil || proof.Status != CredentialUnknown {
		t.Fatalf("%+v %v", proof, err)
	}
}

func TestCredentialOverrideAllowsSelectedHomeAndSQLiteLocation(t *testing.T) {
	home := t.TempDir()
	if key := credentialOverride([]string{"CODEX_HOME=" + home, "CODEX_SQLITE_HOME=/tmp/state"}, home); key != "" {
		t.Fatalf("unexpected override %q", key)
	}
	if key := credentialOverride([]string{"CODEX_HOME=/different"}, home); key != "CODEX_HOME" {
		t.Fatalf("override = %q", key)
	}
}
func TestReusedPIDRefuses(t *testing.T) {
	i, _ := managedFixture(t, "idle")
	run := i.Run
	i.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "lstart=" {
				return []byte("different birth"), nil
			}
		}
		return run(ctx, name, args...)
	}
	if o, err := i.Inspect(context.Background()); err == nil || o.Daemon != switcher.Unknown {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestCustomBinaryAndPrivateServers(t *testing.T) {
	for _, tc := range []struct {
		command  string
		unknown  bool
		warnings int
	}{
		{"/opt/codex-next app-server --listen unix://", true, 1},
		{"/opt/codex-next app-server", false, 1},
		{"/opt/codex-next app-server --listen=stdio://", false, 1},
		{"/opt/codex-next app-server proxy", false, 0},
		{"/opt/codex-next resume abc", false, 1},
	} {
		t.Run(tc.command, func(t *testing.T) {
			home := t.TempDir()
			testNativeAuth(t, home)
			i := Inspector{Home: home, Binary: "/opt/codex-next", Run: func(context.Context, string, ...string) ([]byte, error) {
				return []byte("1234 " + tc.command + "\n"), nil
			}}
			o, err := i.Inspect(context.Background())
			if (err != nil) != tc.unknown || len(o.Warnings) != tc.warnings {
				t.Fatalf("%+v %v", o, err)
			}
		})
	}
}

func TestConfigWorkspaceUnionDoesNotBreakInspection(t *testing.T) {
	for _, value := range []string{`"workspace-a"`, `["workspace-a", "workspace-b"]`} {
		home := t.TempDir()
		raw := "cli_auth_credentials_store = \"file\"\nforced_chatgpt_workspace_id = " + value + "\n"
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if cfg, err := ReadConfig(home); err != nil || cfg.CredentialStore != "file" {
			t.Fatalf("%+v %v", cfg, err)
		}
	}
}

func TestManagedDefaultOpenAIProviderIsRecognized(t *testing.T) {
	i, _ := managedFixture(t, "idle")
	o, err := i.Inspect(context.Background())
	if err != nil || o.Credential.Status != CredentialFileSelected || o.Config.ModelProvider != "openai" {
		t.Fatalf("%+v %v", o.Credential, err)
	}
}

func TestSelectionInspectionDoesNotRequireHealthyThreads(t *testing.T) {
	i, _ := managedFixture(t, "systemError")
	selected, err := i.InspectSelection(context.Background())
	if err != nil || selected.Daemon != switcher.Running || selected.Credential.Status != CredentialFileSelected {
		t.Fatalf("%+v %v", selected.Credential, err)
	}
	if selected.ActivityKnown || selected.ActivityError != "not inspected" {
		t.Fatalf("selection activity = %t %q", selected.ActivityKnown, selected.ActivityError)
	}
	if _, err := i.Inspect(context.Background()); err == nil {
		t.Fatal("switch inspection ignored unhealthy activity")
	}
}

func TestCommandMetadataNeverProvesClientAttachment(t *testing.T) {
	socket := "/tmp/codex/app-server-control/app-server-control.sock"
	for _, tc := range []struct {
		args []string
	}{
		{[]string{"codex", "resume", "thread"}},
		{[]string{"codex", "--remote", "unix://" + socket, "resume", "thread"}},
		{[]string{"codex", "explain", "--remote=unix://" + socket, "safely"}},
		{[]string{"codex", "--", "prompt", "--remote=unix://" + socket}},
	} {
		role, _ := classifyProcessRole(tc.args)
		if role != processCandidate && role != processUncertain {
			t.Fatalf("%v role=%v", tc.args, role)
		}
	}
}

func TestRemoteTextRemainsUnverifiedInObservation(t *testing.T) {
	for _, tc := range []struct {
		command string
		fails   bool
	}{
		{"/bin/codex --remote unix://SOCKET resume synthetic-thread", true},
		{"/bin/codex explain --remote=unix://SOCKET safely", false},
	} {
		home := t.TempDir()
		testNativeAuth(t, home)
		i := Inspector{Home: home}
		command := strings.ReplaceAll(tc.command, "SOCKET", i.Socket())
		i.Run = func(context.Context, string, ...string) ([]byte, error) {
			return []byte("1234 " + command + "\n"), nil
		}
		o, err := i.Inspect(context.Background())
		if (err != nil) != tc.fails || len(o.Clients) != 1 || o.Clients[0].Kind != ClientUnknown || len(o.Warnings) != 1 {
			t.Fatalf("command=%q observation=%+v err=%v", command, o, err)
		}
	}
}

func TestGlobalOptionsDoNotHideAppServer(t *testing.T) {
	for _, command := range []string{
		"/bin/codex app-server --listen unix:///tmp/other.sock",
		"/bin/codex -c model=synthetic app-server --listen unix:///tmp/other.sock",
		`/bin/codex -c model="hello world" app-server --listen unix:///tmp/other.sock`,
		`/bin/codex app-server -c model="hello daemon world" --listen unix:///tmp/other.sock`,
	} {
		home := t.TempDir()
		testNativeAuth(t, home)
		i := Inspector{Home: home, Run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("1234 " + command + "\n"), nil
		}}
		o, err := i.Inspect(context.Background())
		if err == nil || o.Daemon != switcher.Unknown || o.ActivityKnown {
			t.Fatalf("command=%q observation=%+v err=%v", command, o, err)
		}
	}
}

func TestUncertainProcessRoleRemainsInRunningInventory(t *testing.T) {
	i, _ := managedFixture(t, "idle")
	run := i.Run
	i.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		raw, err := run(ctx, name, args...)
		if name == "ps" && len(args) > 0 && args[0] == "-u" {
			raw = append(raw, []byte("987654 /bin/codex --image /tmp/synthetic.png resume synthetic-thread\n")...)
		}
		return raw, err
	}
	o, err := i.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range o.Clients {
		if client.PID == 987654 && client.Kind == ClientUnknown && client.Basis == "command-role-unverified" {
			return
		}
	}
	t.Fatalf("uncertain process missing: clients=%+v warnings=%v", o.Clients, o.Warnings)
}

func TestManagedRootOverrideRemainsFailClosed(t *testing.T) {
	i, _ := managedFixture(t, "idle")
	run := i.Run
	i.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		raw, err := run(ctx, name, args...)
		if name == "ps" && len(args) > 0 && args[0] == "-u" {
			raw = []byte(strings.ReplaceAll(string(raw), "/bin/codex app-server", "/bin/codex -c model=synthetic app-server"))
		}
		return raw, err
	}
	o, err := i.Inspect(context.Background())
	if err != nil || o.Daemon != switcher.Running || o.Credential.Status != CredentialUnknown {
		t.Fatalf("observation=%+v err=%v", o, err)
	}
}

func TestUncertainOtherServerDoesNotPoisonManagedConfig(t *testing.T) {
	i, _ := managedFixture(t, "idle")
	run := i.Run
	i.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		raw, err := run(ctx, name, args...)
		if name == "ps" && len(args) > 0 && args[0] == "-u" {
			raw = append([]byte("987654 /bin/codex -c model=other app-server --listen unix:///tmp/other.sock\n"), raw...)
		}
		return raw, err
	}
	o, err := i.Inspect(context.Background())
	if err != nil || o.Credential.Status != CredentialFileSelected {
		t.Fatalf("observation=%+v err=%v", o, err)
	}
}

func TestTerminalUtilityFlagIsNotAProcessCandidate(t *testing.T) {
	home := t.TempDir()
	testNativeAuth(t, home)
	i := Inspector{Home: home, Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("1234 /bin/codex --version\n"), nil
	}}
	o, err := i.Inspect(context.Background())
	if err != nil || len(o.Clients) != 0 || len(o.Warnings) != 0 {
		t.Fatalf("observation=%+v err=%v", o, err)
	}
}

func TestUtilityLookingPositionalTextRemainsACandidate(t *testing.T) {
	home := t.TempDir()
	testNativeAuth(t, home)
	i := Inspector{Home: home, Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("1235 /bin/codex login status\n"), nil
	}}
	o, err := i.Inspect(context.Background())
	if err != nil || len(o.Clients) != 1 || o.Clients[0].Kind != ClientUnknown {
		t.Fatalf("observation=%+v err=%v", o, err)
	}
}

func TestClientInventoryIsBoundedAndWarningsAreAggregated(t *testing.T) {
	home := t.TempDir()
	testNativeAuth(t, home)
	var processList strings.Builder
	for n := 0; n < maxClientInventory+2; n++ {
		processList.WriteString(strconv.Itoa(1000 + n))
		processList.WriteString(" /bin/codex resume synthetic-thread\n")
	}
	i := Inspector{Home: home, Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(processList.String()), nil
	}}
	o, err := i.Inspect(context.Background())
	if err != nil || len(o.Clients) != maxClientInventory {
		t.Fatalf("clients=%d err=%v", len(o.Clients), err)
	}
	if len(o.Warnings) != 2 || !strings.Contains(o.Warnings[0], "130 Codex process candidates") || !strings.Contains(o.Warnings[1], "limited to 128") {
		t.Fatalf("warnings=%v", o.Warnings)
	}
	for _, warning := range o.Warnings {
		if strings.Contains(warning, "1000") {
			t.Fatalf("warning leaked per-process identity: %q", warning)
		}
	}
}
