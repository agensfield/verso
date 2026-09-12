package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/switcher"
	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	CredentialStore string `toml:"cli_auth_credentials_store" json:"cli_auth_credentials_store"`
	ModelProvider   string `toml:"model_provider" json:"model_provider"`
	SQLiteHome      string `toml:"sqlite_home" json:"sqlite_home"`
}

type Process struct {
	PID     int
	Command string
}
type ProcessRecord struct {
	PID       int    `json:"pid"`
	StartTime string `json:"processStartTime"`
}

type ClientKind string

const (
	ClientAttached   ClientKind = "attached"
	ClientStandalone ClientKind = "standalone"
	ClientUnknown    ClientKind = "unknown"
)

type Client struct {
	PID   int        `json:"pid"`
	Kind  ClientKind `json:"kind"`
	Basis string     `json:"basis"`
}

type CredentialProof struct {
	Status        string                  `json:"status"`
	Basis         string                  `json:"basis,omitempty"`
	Reason        string                  `json:"reason,omitempty"`
	Warning       string                  `json:"warning,omitempty"`
	EffectiveMode string                  `json:"effectiveMode,omitempty"`
	StartupCWD    string                  `json:"startupCwd,omitempty"`
	FileIdentity  accounts.ActiveIdentity `json:"fileIdentity"`
}

// NativeSelection is an exact, credential-free fingerprint of one auth.json
// selection. Capture it after activation and before starting a replacement.
type NativeSelection struct {
	Identity accounts.ActiveIdentity
	Snapshot string
}

const (
	CredentialUnknown      = "unknown"
	CredentialLocalFile    = "local-file-mode"
	CredentialFileSelected = "file-selected"
	CredentialFreshProcess = "fresh-process"
)

// CredentialResolver is the deliberately narrow extension point for proving a
// stopped runtime's future credential mode. The native user config alone is not
// enough: callers must resolve every supported config source for the startup cwd.
type CredentialResolver interface {
	ResolveCredentialConfig(context.Context, CredentialResolveRequest) (CredentialResolution, error)
}

type CredentialResolveRequest struct {
	Home                string
	CWD                 string
	Env                 []string
	EnvironmentComplete bool
	Selection           accounts.ActiveIdentity
	LaunchArgs          []string
}

type CredentialResolution struct {
	Status        string
	EffectiveMode string
	Basis         string
	Snapshot      string
	Warning       string
}

// CredentialResolutionError carries a sanitized capability boundary suitable
// for status output. Other resolver errors remain deliberately opaque.
type CredentialResolutionError struct{ Reason string }

func (e *CredentialResolutionError) Error() string { return e.Reason }

type Observation struct {
	Daemon         switcher.Daemon         `json:"daemon"`
	Home           string                  `json:"codexHome"`
	Version        string                  `json:"version,omitempty"`
	Config         Config                  `json:"config"`
	SelectedFile   accounts.ActiveIdentity `json:"selectedFileIdentity"`
	SelectedEmail  string                  `json:"selectedFileEmail,omitempty"`
	Email          string                  `json:"-"` // compatibility alias for SelectedEmail
	Credential     CredentialProof         `json:"credentialProof"`
	Busy           []string                `json:"busy,omitempty"`
	ActivityKnown  bool                    `json:"activityKnown"`
	ActivityError  string                  `json:"activityError,omitempty"`
	Clients        []Client                `json:"clients"`
	Warnings       []string                `json:"warnings,omitempty"`
	Record         *ProcessRecord          `json:"-"`
	authSnapshot   string
	configSnapshot string
}

// CommandRunner does not return command output in errors, which may contain secrets.
type CommandRunner func(context.Context, string, ...string) ([]byte, error)

func RunCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s command failed", filepath.Base(name))
	}
	return out, nil
}

type Inspector struct {
	Home    string
	Binary  string
	Version string
	Run     CommandRunner
	// Env and LaunchArgs describe the replacement process, not an inferred
	// environment for an already-running server. Fresh verification requires
	// LaunchEnvKnown so an omitted variable cannot be mistaken for absence.
	Env            []string
	CWD            string
	Resolver       CredentialResolver
	LaunchArgs     []string
	LaunchEnvKnown bool
}

func (i Inspector) Socket() string {
	return filepath.Join(i.Home, "app-server-control", "app-server-control.sock")
}
func (i Inspector) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if i.Run != nil {
		return i.Run(ctx, name, args...)
	}
	return RunCommand(ctx, name, args...)
}

func ReadConfig(home string) (Config, error) {
	raw, err := readRegular(filepath.Join(home, "config.toml"), 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, errors.New("cannot read Codex config safely")
	}
	var cfg Config
	if toml.Unmarshal(raw, &cfg) != nil {
		return Config{}, errors.New("invalid Codex config.toml")
	}
	return cfg, nil
}

// ReadIdentity parses local native metadata, not a daemon identity attestation.
func ReadIdentity(home string) (accounts.ActiveIdentity, string, error) {
	identity, email, _, err := readIdentitySnapshot(home)
	return identity, email, err
}

func ReadNativeSelection(home string) (NativeSelection, error) {
	identity, _, snapshot, err := readIdentitySnapshot(home)
	if err != nil {
		return NativeSelection{}, err
	}
	return NativeSelection{Identity: identity, Snapshot: snapshot}, nil
}

func readIdentitySnapshot(home string) (accounts.ActiveIdentity, string, string, error) {
	raw, err := readRegular(filepath.Join(home, "auth.json"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return accounts.ActiveIdentity{Known: true}, "", fmt.Sprintf("%x", sha256.Sum256(nil)), nil
	}
	if err != nil {
		return accounts.ActiveIdentity{}, "", "", errors.New("cannot read native credential file safely")
	}
	auth, err := accounts.ParseNativeAuth(raw)
	if err != nil {
		return accounts.ActiveIdentity{}, "", "", err
	}
	return accounts.ActiveIdentity{Known: true, UserID: auth.UserID, AccountID: auth.AccountID}, auth.Email, fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func readRegular(name string, max int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, errors.New("unexpected file type or size")
	}
	return os.ReadFile(name)
}

func (i Inspector) Inspect(ctx context.Context) (Observation, error) {
	return i.inspect(ctx, true)
}

// InspectSelection establishes native selection/config evidence without asking
// whether conversations can be stopped. Account import does not stop them.
func (i Inspector) InspectSelection(ctx context.Context) (Observation, error) {
	return i.inspect(ctx, false)
}

func (i Inspector) inspect(ctx context.Context, activity bool) (Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	o := Observation{
		Daemon:        switcher.Unknown,
		Home:          i.Home,
		ActivityError: "not inspected",
		Clients:       make([]Client, 0),
	}
	if !filepath.IsAbs(i.Home) {
		return o, errors.New("Codex home must be absolute")
	}
	cfg, err := ReadConfig(i.Home)
	if err != nil {
		o.Credential = unknownCredential("user config metadata is unavailable")
	} else {
		o.Config = cfg
	}
	o.SelectedFile, o.SelectedEmail, o.authSnapshot, err = readIdentitySnapshot(i.Home)
	o.Email = o.SelectedEmail
	if err != nil {
		o.Credential = unknownCredential("native credential metadata is unavailable")
	}
	if o.Credential.Status == "" {
		o.Credential = unknownCredential("effective credential mode has not been resolved")
	}
	if key := credentialOverride(i.Env, i.Home); key != "" {
		o.Credential = unknownCredential("alternative auth or runtime override " + key + " is present")
	}
	processes, err := i.processes(ctx)
	if err != nil {
		return o, err
	}
	servers := 0
	var publicServers []Process
	runtimeRoleUncertain := false
	clientCounts := make(map[ClientKind]int)
	clientsSeen := 0
	addClient := func(client Client) {
		clientsSeen++
		clientCounts[client.Kind]++
		if len(o.Clients) < maxClientInventory {
			o.Clients = append(o.Clients, client)
		}
	}
	for _, p := range processes {
		fields := strings.Fields(p.Command)
		if len(fields) == 0 || !i.isCodex(fields[0]) {
			continue
		}
		role, roleArgs := classifyProcessRole(fields)
		switch role {
		case processIgnored:
			continue
		case processUncertain:
			runtimeRoleUncertain = true
			continue
		case processServer:
			if privateServer(roleArgs) {
				addClient(Client{PID: p.PID, Kind: ClientStandalone, Basis: "private-app-server-process"})
			} else {
				servers++
				publicServers = append(publicServers, p)
			}
			continue
		case processCandidate:
			addClient(Client{PID: p.PID, Kind: ClientUnknown, Basis: "command-metadata-unverified"})
		}
	}
	_, socketErr := os.Lstat(i.Socket())
	if socketErr != nil && !errors.Is(socketErr, os.ErrNotExist) {
		return o, errors.New("cannot inspect daemon socket")
	}
	record, err := i.readRecord()
	if err != nil {
		return o, err
	}
	for _, server := range publicServers {
		if record == nil || server.PID != record.PID {
			addClient(Client{PID: server.PID, Kind: ClientStandalone, Basis: "independent-app-server-process"})
		}
	}
	o.Warnings = append(o.Warnings, clientWarnings(clientCounts)...)
	if clientsSeen > len(o.Clients) {
		o.Warnings = append(o.Warnings, fmt.Sprintf("Codex client inventory is limited to %d entries", maxClientInventory))
	}
	alive := false
	if record != nil {
		for _, p := range processes {
			if p.PID == record.PID {
				birth, e := i.command(ctx, "ps", "-p", strconv.Itoa(record.PID), "-o", "lstart=")
				if e != nil {
					return o, errors.New("cannot verify daemon birth identity")
				}
				if normalSpace(string(birth)) != normalSpace(record.StartTime) {
					return o, errors.New("daemon PID record does not match process birth identity")
				}
				fields := strings.Fields(p.Command)
				role, _ := classifyProcessRole(fields)
				if len(fields) < 2 || !i.isCodex(fields[0]) || role != processServer {
					return o, errors.New("managed PID is not a recognized Codex server")
				}
				alive = true
			}
		}
	}
	if !alive {
		if socketErr == nil || servers > 0 || runtimeRoleUncertain {
			return o, errors.New("socket or unmanaged app-server exists without a verified managed process")
		}
		o.Daemon = switcher.Stopped
		if activity {
			o.ActivityKnown = true
			o.ActivityError = ""
		}
		if i.Resolver == nil {
			o.Credential = unknownCredential("stopped runtime has no complete config resolver")
		} else if strings.TrimSpace(i.CWD) == "" || !filepath.IsAbs(i.CWD) {
			o.Credential = unknownCredential("stopped runtime startup cwd is unavailable")
		} else if credentialOverride(i.Env, i.Home) == "" && o.SelectedFile.Known {
			resolved, resolveErr := i.Resolver.ResolveCredentialConfig(ctx, CredentialResolveRequest{Home: i.Home, CWD: i.CWD, Env: i.Env, EnvironmentComplete: i.LaunchEnvKnown, Selection: o.SelectedFile, LaunchArgs: i.LaunchArgs})
			if resolveErr != nil {
				reason := "stopped runtime config sources could not be resolved"
				var capability *CredentialResolutionError
				if errors.As(resolveErr, &capability) {
					reason = capability.Reason
				}
				o.Credential = unknownCredential(reason)
			} else if resolved.EffectiveMode != "file" || resolved.Basis == "" || resolved.Snapshot == "" {
				o.Credential = unknownCredential("stopped runtime is not proven file-backed")
			} else {
				status := resolved.Status
				if status == "" {
					status = CredentialFileSelected
				}
				if status != CredentialLocalFile && status != CredentialFileSelected {
					o.Credential = unknownCredential("stopped runtime resolver returned an unsupported proof status")
					return o, nil
				}
				if status == CredentialLocalFile && (o.SelectedFile.UserID != "" || o.SelectedFile.AccountID != "") && resolved.Warning == "" {
					o.Credential = unknownCredential("logged-in local file mode requires an unresolved cloud warning")
					return o, nil
				}
				o.Config.CredentialStore = resolved.EffectiveMode
				o.configSnapshot = resolved.Snapshot
				o.Credential = CredentialProof{Status: status, Basis: resolved.Basis, Warning: resolved.Warning, EffectiveMode: resolved.EffectiveMode, StartupCWD: i.CWD, FileIdentity: o.SelectedFile}
			}
		}
		return o, nil
	}
	if socketErr != nil {
		return o, errors.New("managed daemon exists but socket is unreachable")
	}
	owned, err := i.ownsSocket(ctx, record.PID, i.Socket())
	if err != nil || !owned {
		return o, errors.New("cannot correlate the Codex socket with the managed process")
	}
	rpc, err := DialRPC(ctx, i.Socket(), i.Version)
	if err != nil {
		return o, err
	}
	defer rpc.Close()
	home, err := filepath.EvalSymlinks(i.Home)
	if err != nil {
		return o, errors.New("cannot canonicalize Codex home")
	}
	serverHome, err := filepath.EvalSymlinks(rpc.Info.CodexHome)
	if err != nil || home != serverHome {
		return o, errors.New("daemon reports a different Codex home")
	}
	// Runtime identity is established independently of thread activity. Keep
	// this evidence when the bounded activity inventory is unavailable.
	o.Daemon, o.Record, o.Version = switcher.Running, record, rpc.Info.UserAgent
	managedOverride := ""
	for _, server := range publicServers {
		if server.PID == record.PID {
			fields := strings.Fields(server.Command)
			managedOverride = commandConfigOverride(fields[1:])
			break
		}
	}
	cwd, cwdErr := i.processCWD(ctx, record.PID)
	if cwdErr != nil || !filepath.IsAbs(cwd) {
		o.Credential = unknownCredential("managed process startup cwd is unavailable")
	} else if credentialOverride(i.Env, i.Home) == "" && managedOverride == "" && o.SelectedFile.Known {
		cfg, snapshot, proof := resolveLiveCredential(ctx, rpc, cwd, o.SelectedFile)
		o.Config, o.configSnapshot, o.Credential = cfg, snapshot, proof
	} else if managedOverride != "" {
		o.Credential = unknownCredential("managed process uses runtime config override " + managedOverride)
	}
	var busy []string
	if activity {
		busy, err = loadedBusy(ctx, rpc)
		if err != nil {
			o.ActivityError = "loaded thread activity is unavailable"
			return o, err
		}
		o.ActivityKnown = true
		o.ActivityError = ""
	}
	o.Busy = busy
	o.Warnings = append(o.Warnings, "background activity is not completely observable; native shutdown may interrupt it")
	return o, nil
}

const maxClientInventory = 128

func clientWarnings(counts map[ClientKind]int) []string {
	var warnings []string
	if counts[ClientStandalone] > 0 {
		warnings = append(warnings, fmt.Sprintf("%d standalone Codex runtime(s) may retain previous credentials; reopen them after switching", counts[ClientStandalone]))
	}
	if counts[ClientUnknown] > 0 {
		warnings = append(warnings, fmt.Sprintf("attachment could not be verified for %d Codex process candidate(s); some may need reopening after switching", counts[ClientUnknown]))
	}
	return warnings
}

func unknownCredential(reason string) CredentialProof {
	return CredentialProof{Status: CredentialUnknown, Reason: reason}
}

func credentialOverride(env []string, home string) string {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		if key == "CODEX_HOME" {
			if !filepath.IsAbs(value) || filepath.Clean(value) != filepath.Clean(home) {
				return key
			}
			continue
		}
		if key == "CODEX_ACCESS_TOKEN" || key == "CODEX_API_KEY" || key == "OPENAI_API_KEY" || key == "CODEX_EXEC_SERVER_URL" {
			return key
		}
	}
	return ""
}

func commandConfigOverride(args []string) string {
	for _, arg := range args {
		if arg == "-c" || arg == "--config" || strings.HasPrefix(arg, "--config=") || arg == "--profile" || strings.HasPrefix(arg, "--profile=") || arg == "--auth" || strings.HasPrefix(arg, "--auth=") {
			return strings.SplitN(arg, "=", 2)[0]
		}
	}
	return ""
}

type configLayer struct {
	Name struct {
		Type string `json:"type"`
	} `json:"name"`
	Version string `json:"version"`
}

func resolveLiveCredential(ctx context.Context, rpc *RPC, cwd string, selected accounts.ActiveIdentity) (Config, string, CredentialProof) {
	var reply struct {
		Config Config        `json:"config"`
		Layers []configLayer `json:"layers"`
	}
	params := map[string]any{"includeLayers": true, "cwd": cwd}
	if err := rpc.Call(ctx, "config/read", params, &reply); err != nil {
		return Config{}, "", unknownCredential("effective config layers are unavailable")
	}
	var requirements struct {
		Requirements *struct {
			CredentialStore *string `json:"cliAuthCredentialsStore"`
		} `json:"requirements"`
	}
	if err := rpc.Call(ctx, "configRequirements/read", nil, &requirements); err != nil {
		return reply.Config, "", unknownCredential("effective config requirements are unavailable")
	}
	if reply.Config.CredentialStore != "file" {
		return reply.Config, "", unknownCredential("effective credential mode is not file")
	}
	// Config/read preserves an omitted model_provider; the native runtime
	// resolves that omission to its built-in OpenAI provider.
	if reply.Config.ModelProvider == "" {
		reply.Config.ModelProvider = "openai"
	}
	if reply.Config.ModelProvider != "openai" {
		return reply.Config, "", unknownCredential("effective model provider does not use native OpenAI credentials")
	}
	if requirements.Requirements != nil && requirements.Requirements.CredentialStore != nil && *requirements.Requirements.CredentialStore != "file" {
		return reply.Config, "", unknownCredential("credential requirement disagrees with effective config")
	}
	if len(reply.Layers) == 0 {
		return reply.Config, "", unknownCredential("effective config returned no layer evidence")
	}
	known := map[string]bool{"packagedDefaults": true, "mdm": true, "system": true, "enterpriseManaged": true, "user": true, "project": true, "sessionFlags": true, "legacyManagedConfigTomlFromFile": true, "legacyManagedConfigTomlFromMdm": true}
	for _, layer := range reply.Layers {
		if !known[layer.Name.Type] || layer.Version == "" {
			return reply.Config, "", unknownCredential("effective config contains an unsupported layer")
		}
	}
	raw, err := json.Marshal(struct {
		Mode        string        `json:"mode"`
		CWD         string        `json:"cwd"`
		Layers      []configLayer `json:"layers"`
		Requirement any           `json:"requirement"`
	}{reply.Config.CredentialStore, cwd, reply.Layers, requirements.Requirements})
	if err != nil {
		return reply.Config, "", unknownCredential("effective config snapshot is unavailable")
	}
	snapshot := fmt.Sprintf("%x", sha256.Sum256(raw))
	return reply.Config, snapshot, CredentialProof{Status: CredentialFileSelected, Basis: "effective-config-and-native-file", EffectiveMode: "file", StartupCWD: cwd, FileIdentity: selected}
}

// VerifyFreshSelection is for the post-start phase of a switch. It attests only
// that a fresh managed process started from the reread target file under the
// resolved file-backed config; it does not claim the server exposed native IDs.
func (i Inspector) VerifyFreshSelection(ctx context.Context, previous ProcessRecord, expected NativeSelection) (CredentialProof, error) {
	if !expected.Identity.Known || expected.Identity.UserID == "" || expected.Identity.AccountID == "" || expected.Snapshot == "" {
		if !(expected.Identity.Known && expected.Identity.UserID == "" && expected.Identity.AccountID == "" && expected.Snapshot != "") {
			return unknownCredential("expected native identity is incomplete"), errors.New("cannot verify an incomplete target identity")
		}
	}
	if !i.LaunchEnvKnown {
		return unknownCredential("replacement launch environment was not supplied"), errors.New("replacement launch environment is unknown")
	}
	o, err := i.Inspect(ctx)
	if err != nil {
		return o.Credential, err
	}
	if o.Daemon != switcher.Running || o.Record == nil {
		return unknownCredential("fresh managed process is not running"), errors.New("fresh managed process is not running")
	}
	if o.Record.PID == previous.PID && normalSpace(o.Record.StartTime) == normalSpace(previous.StartTime) {
		return unknownCredential("managed process was not replaced"), errors.New("managed process was not replaced")
	}
	if o.Credential.Status != CredentialFileSelected || o.configSnapshot == "" {
		return o.Credential, errors.New("fresh process credential mode is unproven")
	}
	selected, _, authSnapshot, err := readIdentitySnapshot(i.Home)
	if err != nil || authSnapshot == "" || authSnapshot != expected.Snapshot || authSnapshot != o.authSnapshot || selected.UserID != expected.Identity.UserID || selected.AccountID != expected.Identity.AccountID {
		return unknownCredential("installed native identity changed or does not match target"), errors.New("installed native identity does not match target")
	}
	processes, err := i.processes(ctx)
	if err != nil {
		return unknownCredential("old process exit cannot be established"), err
	}
	for _, process := range processes {
		if process.PID != previous.PID {
			continue
		}
		birth, birthErr := i.command(ctx, "ps", "-p", strconv.Itoa(previous.PID), "-o", "lstart=")
		if birthErr != nil {
			return unknownCredential("old process exit cannot be established"), errors.New("old process exit cannot be established")
		}
		if normalSpace(string(birth)) == normalSpace(previous.StartTime) {
			return unknownCredential("old managed process is still running"), errors.New("old managed process is still running")
		}
	}
	proof := o.Credential
	proof.Status = CredentialFreshProcess
	proof.Basis = "fresh-managed-process-and-native-file"
	return proof, nil
}

func (i Inspector) readRecord() (*ProcessRecord, error) {
	raw, err := readRegular(filepath.Join(i.Home, "app-server-daemon", "app-server.pid"), 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot read daemon process record")
	}
	var record ProcessRecord
	if json.Unmarshal(raw, &record) != nil || record.PID <= 0 || strings.TrimSpace(record.StartTime) == "" {
		return nil, errors.New("invalid daemon process record")
	}
	return &record, nil
}

func (i Inspector) processes(ctx context.Context) ([]Process, error) {
	raw, err := i.command(ctx, "ps", "-u", strconv.Itoa(os.Geteuid()), "-o", "pid=", "-o", "command=")
	if err != nil {
		return nil, errors.New("cannot inspect local processes; daemon state is unknown")
	}
	var out []Process
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, errors.New("invalid process metadata")
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, errors.New("invalid process identity")
		}
		out = append(out, Process{pid, strings.TrimSpace(strings.TrimPrefix(line, fields[0]))})
	}
	return out, nil
}
func normalSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

type processRole uint8

const (
	processUncertain processRole = iota
	processIgnored
	processCandidate
	processServer
)

var rootOptionsWithValue = map[string]bool{
	"-c": true, "--config": true, "--enable": true, "--disable": true,
	"--remote": true, "--remote-auth-token-env": true,
	"-m": true, "--model": true, "--local-provider": true,
	"-p": true, "--profile": true, "-s": true, "--sandbox": true,
	"-C": true, "--cd": true, "--add-dir": true,
	"-a": true, "--ask-for-approval": true,
}

var rootBooleanOptions = map[string]bool{
	"--strict-config": true, "--oss": true, "--approve-for-me": true,
	"--dangerously-bypass-approvals-and-sandbox": true,
	"--dangerously-bypass-hook-trust":            true, "--worktree": true,
	"--search": true, "--no-alt-screen": true,
}

var utilityCommands = map[string]bool{
	"login": true, "logout": true, "mcp": true, "plugin": true,
	"remote-control": true, "app": true, "completion": true, "update": true,
	"doctor": true, "sandbox": true, "apply": true, "a": true,
	"features": true, "help": true,
}

// ps command= is flattened text, not an argv vector. This parser recognizes
// only enough stable root syntax to avoid counting known utility processes and
// to fail closed on a possible app-server. It never proves client transport.
func classifyProcessRole(fields []string) (processRole, []string) {
	for n := 1; n < len(fields); n++ {
		arg := fields[n]
		if arg == "--" {
			return processCandidate, nil
		}
		if rootOptionsWithValue[arg] {
			if n+1 >= len(fields) {
				return processUncertain, nil
			}
			n++
			continue
		}
		if arg == "-i" || arg == "--image" {
			return processUncertain, nil
		}
		if arg == "-h" || arg == "--help" || arg == "-V" || arg == "--version" {
			return processIgnored, nil
		}
		if rootBooleanOptions[arg] {
			continue
		}
		if strings.HasPrefix(arg, "--") && strings.Contains(arg, "=") {
			key, _, _ := strings.Cut(arg, "=")
			if rootOptionsWithValue[key] {
				continue
			}
			return processUncertain, nil
		}
		if strings.HasPrefix(arg, "-") {
			return processUncertain, nil
		}
		switch arg {
		case "app-server":
			args := fields[n+1:]
			for _, candidate := range args {
				if candidate == "daemon" || candidate == "proxy" || strings.HasPrefix(candidate, "generate-") {
					return processIgnored, nil
				}
			}
			return processServer, args
		default:
			if utilityCommands[arg] {
				return processIgnored, nil
			}
			return processCandidate, nil
		}
	}
	return processCandidate, nil
}

func loadedBusy(ctx context.Context, rpc *RPC) ([]string, error) {
	var busy []string
	cursor := ""
	seen := make(map[string]bool)
	for page := 0; page < 1000; page++ {
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var reply struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if err := rpc.Call(ctx, "thread/loaded/list", params, &reply); err != nil {
			return nil, err
		}
		for _, id := range reply.Data {
			var thread struct {
				Thread struct {
					Status struct {
						Type  string   `json:"type"`
						Flags []string `json:"activeFlags"`
					} `json:"status"`
				} `json:"thread"`
			}
			if err := rpc.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &thread); err != nil {
				return nil, errors.New("cannot inspect a loaded thread; runtime state is unknown")
			}
			switch thread.Thread.Status.Type {
			case "active":
				busy = append(busy, id)
			case "idle", "notLoaded":
			default:
				return nil, errors.New("loaded thread has an unknown/error status")
			}
		}
		if reply.NextCursor == nil || *reply.NextCursor == "" {
			return busy, nil
		}
		cursor = *reply.NextCursor
		if seen[cursor] {
			return nil, errors.New("daemon repeated thread cursor")
		}
		seen[cursor] = true
	}
	return nil, errors.New("loaded thread inventory exceeded safety limit")
}

// ps command text is only a discovery hint; managed ownership additionally needs
// the recorded birth identity and socket correlation.
func (i Inspector) isCodex(executable string) bool {
	if filepath.Base(executable) == "codex" {
		return true
	}
	return i.Binary != "" && filepath.Base(executable) == filepath.Base(i.Binary)
}

func privateServer(args []string) bool {
	for n, arg := range args {
		if arg == "--listen" {
			return n+1 < len(args) && args[n+1] == "stdio://"
		}
		if strings.HasPrefix(arg, "--listen=") {
			return arg == "--listen=stdio://"
		}
	}
	return true // The native default is stdio, including explicit --stdio.
}
