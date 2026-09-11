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
	if err != nil || o.Daemon != switcher.Stopped || o.Active.AccountID != "account-a" {
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
	_, err := i.Inspect(context.Background())
	if err == nil || strings.Contains(err.Error(), "do-not-echo") {
		t.Fatal(err)
	}
}

func managedFixture(t *testing.T, status string) (Inspector, func()) {
	t.Helper()
	home := t.TempDir()
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
			return []byte("p" + strconv.Itoa(pid) + "\nn" + i.Socket() + "\n"), nil
		}
		for _, arg := range args {
			if arg == "lstart=" {
				return []byte(birth), nil
			}
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
				reply = map[string]any{"config": Config{CredentialStore: "file", ModelProvider: "openai"}}
			case "account/read":
				reply = map[string]any{"account": map[string]string{"type": "chatgpt", "email": "a@example.test"}}
			case "thread/loaded/list":
				reply = map[string]any{"data": []string{"thread-a"}, "nextCursor": nil}
			case "thread/read":
				reply = map[string]any{"thread": map[string]any{"status": map[string]any{"type": status, "activeFlags": []string{"waitingOnApproval"}}}}
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
				if err == nil || o.Daemon != switcher.Unknown {
					t.Fatalf("%+v %v", o, err)
				}
				return
			}
			if err != nil || o.Daemon != switcher.Running {
				t.Fatalf("%+v %v", o, err)
			}
			if (status == "active") != (len(o.Busy) == 1) {
				t.Fatalf("busy=%v", o.Busy)
			}
		})
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
		{"/opt/codex-next app-server --listen unix://", true, 0},
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
