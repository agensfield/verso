package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agensfield/verso/internal/accounts"
)

func absentSources(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

func stoppedRequest(t *testing.T) CredentialResolveRequest {
	t.Helper()
	home := t.TempDir()
	cwd := t.TempDir()
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return CredentialResolveRequest{Home: home, CWD: cwd, EnvironmentComplete: true, Selection: accounts.ActiveIdentity{Known: true}}
}

func TestLocalResolverSupportsCommonLoggedOutLinuxAndMacPaths(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			req := stoppedRequest(t)
			resolver := localCredentialResolver{goos: goos, lstat: absentSources, run: RunCommand, managedPrefs: func(context.Context, CommandRunner) (bool, error) { return false, nil }}
			got, err := resolver.ResolveCredentialConfig(context.Background(), req)
			if err != nil || got.Status != CredentialLocalFile || got.EffectiveMode != "file" || got.Snapshot == "" || got.Warning != "" {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestLocalResolverLabelsLoggedInCloudBoundary(t *testing.T) {
	req := stoppedRequest(t)
	req.Selection.UserID, req.Selection.AccountID = "user", "account"
	resolver := localCredentialResolver{goos: "linux", lstat: absentSources, run: RunCommand}
	got, err := resolver.ResolveCredentialConfig(context.Background(), req)
	if err != nil || got.Status != CredentialLocalFile || got.Warning == "" || got.Basis == "" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestLocalResolverAllowsInactiveProfilesAndNativeConfigAncestor(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".codex")
	cwd := filepath.Join(root, "repos", "project")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := "cli_auth_credentials_store = \"file\"\n[profiles.saved]\nmodel = \"gpt-test\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	nativeConfig := filepath.Join(home, "config.toml")
	resolver := localCredentialResolver{goos: "linux", lstat: func(path string) (os.FileInfo, error) {
		if path == nativeConfig {
			return fakeFileInfo{}, nil
		}
		return nil, os.ErrNotExist
	}, run: RunCommand}
	req := CredentialResolveRequest{Home: home, CWD: cwd, EnvironmentComplete: true, Selection: accounts.ActiveIdentity{Known: true}}
	got, err := resolver.ResolveCredentialConfig(context.Background(), req)
	if err != nil || got.Status != CredentialLocalFile {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestLocalResolverRefusesEveryUnresolvedInfluence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CredentialResolveRequest)
		stat   func(string) (os.FileInfo, error)
	}{
		{"runtime-override", func(r *CredentialResolveRequest) {
			r.LaunchArgs = []string{"--config", "cli_auth_credentials_store=keyring"}
		}, absentSources},
		{"different-home", func(r *CredentialResolveRequest) { r.Env = []string{"CODEX_HOME=/different"} }, absentSources},
		{"system-layer", func(*CredentialResolveRequest) {}, func(path string) (os.FileInfo, error) {
			if path == "/etc/codex/config.toml" {
				return fakeFileInfo{}, nil
			}
			return nil, os.ErrNotExist
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := stoppedRequest(t)
			tc.mutate(&req)
			resolver := localCredentialResolver{goos: "linux", lstat: tc.stat, run: RunCommand}
			if _, err := resolver.ResolveCredentialConfig(context.Background(), req); err == nil {
				t.Fatal("expected conservative refusal")
			}
		})
	}
}

func TestLocalResolverRefusesProfileProjectAndMacManagedSources(t *testing.T) {
	t.Run("profile", func(t *testing.T) {
		req := stoppedRequest(t)
		if err := os.WriteFile(filepath.Join(req.Home, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\nprofile = \"work\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		resolver := localCredentialResolver{goos: "linux", lstat: absentSources, run: RunCommand}
		if _, err := resolver.ResolveCredentialConfig(context.Background(), req); err == nil {
			t.Fatal("expected profile refusal")
		}
	})
	t.Run("project", func(t *testing.T) {
		req := stoppedRequest(t)
		project := filepath.Join(req.CWD, ".codex", "config.toml")
		resolver := localCredentialResolver{goos: "linux", lstat: func(path string) (os.FileInfo, error) {
			if path == project {
				return fakeFileInfo{}, nil
			}
			return nil, os.ErrNotExist
		}, run: RunCommand}
		if _, err := resolver.ResolveCredentialConfig(context.Background(), req); err == nil {
			t.Fatal("expected project config refusal")
		}
	})
	t.Run("mac-managed-preferences", func(t *testing.T) {
		req := stoppedRequest(t)
		resolver := localCredentialResolver{goos: "darwin", lstat: absentSources, run: RunCommand, managedPrefs: func(context.Context, CommandRunner) (bool, error) { return true, nil }}
		if _, err := resolver.ResolveCredentialConfig(context.Background(), req); err == nil {
			t.Fatal("expected managed preference refusal")
		}
	})
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "config.toml" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0600 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }
