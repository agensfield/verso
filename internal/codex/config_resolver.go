package codex

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// NewLocalCredentialResolver proves the small stopped-runtime configuration
// subset that can be established without starting Codex or contacting a server.
// A logged-in selection is intentionally refused because its enterprise cloud
// bundle does not exist on disk and may change the final startup config.
func NewLocalCredentialResolver(run CommandRunner) CredentialResolver {
	if run == nil {
		run = RunCommand
	}
	return localCredentialResolver{goos: runtime.GOOS, lstat: os.Lstat, run: run, managedPrefs: managedPreferencesPresent}
}

type localCredentialResolver struct {
	goos         string
	lstat        func(string) (os.FileInfo, error)
	run          CommandRunner
	managedPrefs func(context.Context, CommandRunner) (bool, error)
}

type localUserConfig struct {
	CredentialStore string         `toml:"cli_auth_credentials_store"`
	ModelProvider   string         `toml:"model_provider"`
	Profile         string         `toml:"profile"`
	Profiles        map[string]any `toml:"profiles"`
}

func (r localCredentialResolver) ResolveCredentialConfig(ctx context.Context, req CredentialResolveRequest) (CredentialResolution, error) {
	if !filepath.IsAbs(req.Home) || !filepath.IsAbs(req.CWD) {
		return CredentialResolution{}, resolutionError("stopped runtime paths are not absolute")
	}
	if !req.EnvironmentComplete {
		return CredentialResolution{}, resolutionError("replacement launch environment is unknown")
	}
	if key := credentialOverride(req.Env, req.Home); key != "" {
		return CredentialResolution{}, resolutionError("launch environment contains unsupported override " + key)
	}
	if override := commandConfigOverride(req.LaunchArgs); override != "" {
		return CredentialResolution{}, resolutionError("launch arguments contain unsupported override " + override)
	}
	if req.Selection.UserID != "" || req.Selection.AccountID != "" {
		return CredentialResolution{}, resolutionError("logged-in stopped runtime may receive an unobservable enterprise cloud config")
	}

	raw, err := readRegular(filepath.Join(req.Home, "config.toml"), 4<<20)
	if err != nil {
		return CredentialResolution{}, resolutionError("explicit user config is unavailable")
	}
	var user localUserConfig
	if toml.Unmarshal(raw, &user) != nil {
		return CredentialResolution{}, resolutionError("invalid user config")
	}
	if user.CredentialStore != "file" {
		return CredentialResolution{}, resolutionError("user config does not explicitly select file credentials")
	}
	if user.ModelProvider != "" && user.ModelProvider != "openai" {
		return CredentialResolution{}, resolutionError("user config selects a non-native model provider")
	}
	if user.Profile != "" || len(user.Profiles) != 0 {
		return CredentialResolution{}, resolutionError("user config profiles require full Codex resolution")
	}

	for _, path := range []string{
		"/etc/codex/config.toml",
		"/etc/codex/requirements.toml",
		"/etc/codex/managed_config.toml",
	} {
		present, statErr := r.present(path)
		if statErr != nil {
			return CredentialResolution{}, resolutionError("system config presence is unknown")
		}
		if present {
			return CredentialResolution{}, resolutionError("system or managed config requires full Codex resolution")
		}
	}

	cwd, err := filepath.EvalSymlinks(req.CWD)
	if err != nil {
		return CredentialResolution{}, resolutionError("startup cwd cannot be canonicalized")
	}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		present, statErr := r.present(filepath.Join(dir, ".codex", "config.toml"))
		if statErr != nil {
			return CredentialResolution{}, resolutionError("project config presence is unknown")
		}
		if present {
			return CredentialResolution{}, resolutionError("project config requires full Codex resolution")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}

	if r.goos == "darwin" {
		probe := r.managedPrefs
		if probe == nil {
			probe = managedPreferencesPresent
		}
		managed, prefErr := probe(ctx, r.run)
		if prefErr != nil {
			return CredentialResolution{}, resolutionError("macOS managed preference presence is unknown")
		}
		if managed {
			return CredentialResolution{}, resolutionError("macOS managed preferences require full Codex resolution")
		}
	}

	snapshotInput := strings.Join([]string{req.Home, cwd, user.CredentialStore, user.ModelProvider, fmt.Sprintf("%x", sha256.Sum256(raw))}, "\x00")
	return CredentialResolution{
		EffectiveMode: "file",
		Basis:         "local-files-no-managed-sources-logged-out",
		Snapshot:      fmt.Sprintf("%x", sha256.Sum256([]byte(snapshotInput))),
	}, nil
}

func resolutionError(reason string) error { return &CredentialResolutionError{Reason: reason} }

func (r localCredentialResolver) present(path string) (bool, error) {
	_, err := r.lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
