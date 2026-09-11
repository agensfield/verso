package codex

import (
	"context"
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
type Observation struct {
	Daemon   switcher.Daemon         `json:"daemon"`
	Home     string                  `json:"codexHome"`
	Version  string                  `json:"version,omitempty"`
	Config   Config                  `json:"config"`
	Active   accounts.ActiveIdentity `json:"activeIdentity"`
	Email    string                  `json:"email,omitempty"`
	Busy     []string                `json:"busy,omitempty"`
	Warnings []string                `json:"warnings,omitempty"`
	Record   *ProcessRecord          `json:"-"`
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
	Env     []string
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
	raw, err := readRegular(filepath.Join(home, "auth.json"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return accounts.ActiveIdentity{Known: true}, "", nil
	}
	if err != nil {
		return accounts.ActiveIdentity{}, "", errors.New("cannot read native credential file safely")
	}
	auth, err := accounts.ParseNativeAuth(raw)
	if err != nil {
		return accounts.ActiveIdentity{}, "", err
	}
	return accounts.ActiveIdentity{Known: true, UserID: auth.UserID, AccountID: auth.AccountID}, auth.Email, nil
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	o := Observation{Daemon: switcher.Unknown, Home: i.Home}
	if !filepath.IsAbs(i.Home) {
		return o, errors.New("Codex home must be absolute")
	}
	cfg, err := ReadConfig(i.Home)
	if err != nil {
		return o, err
	}
	o.Config = cfg
	o.Active, o.Email, err = ReadIdentity(i.Home)
	if err != nil {
		return o, err
	}
	for _, entry := range i.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && value != "" && (key == "CODEX_ACCESS_TOKEN" || key == "OPENAI_API_KEY" || key == "CODEX_EXEC_SERVER_URL") {
			return o, fmt.Errorf("alternative auth/runtime override %s is present; cannot establish native ownership", key)
		}
	}
	processes, err := i.processes(ctx)
	if err != nil {
		return o, err
	}
	servers := 0
	for _, p := range processes {
		fields := strings.Fields(p.Command)
		if len(fields) == 0 || !i.isCodex(fields[0]) {
			continue
		}
		if len(fields) > 1 && fields[1] == "app-server" {
			if len(fields) > 2 && (fields[2] == "daemon" || fields[2] == "proxy" || strings.HasPrefix(fields[2], "generate-")) {
				continue
			}
			if privateServer(fields[2:]) {
				o.Warnings = append(o.Warnings, fmt.Sprintf("private Codex app-server %d must be restarted after switching", p.PID))
			} else {
				servers++
			}
			continue
		}
		if !explicitRemote(fields, i.Socket()) {
			o.Warnings = append(o.Warnings, fmt.Sprintf("Codex process %d may be standalone; reopen it after switching", p.PID))
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
				if len(fields) < 2 || !i.isCodex(fields[0]) || fields[1] != "app-server" {
					return o, errors.New("managed PID is not a recognized Codex server")
				}
				alive = true
			}
		}
	}
	if !alive {
		if socketErr == nil || servers > 0 {
			return o, errors.New("socket or unmanaged app-server exists without a verified managed process")
		}
		o.Daemon = switcher.Stopped
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
	var configReply struct {
		Config Config `json:"config"`
	}
	if err := rpc.Call(ctx, "config/read", map[string]any{"includeLayers": false}, &configReply); err != nil {
		return o, err
	}
	o.Config = configReply.Config
	var accountReply struct {
		Account *struct {
			Type  string `json:"type"`
			Email string `json:"email"`
		} `json:"account"`
	}
	if err := rpc.Call(ctx, "account/read", map[string]bool{"refreshToken": false}, &accountReply); err != nil {
		return o, err
	}
	if accountReply.Account == nil {
		if o.Active.UserID != "" {
			return o, errors.New("daemon is logged out but credential file selects an account")
		}
	} else if accountReply.Account.Type != "chatgpt" || accountReply.Account.Email != o.Email {
		return o, errors.New("daemon account does not agree with native credential metadata")
	}
	busy, err := loadedBusy(ctx, rpc)
	if err != nil {
		return o, err
	}
	if servers > 1 {
		o.Warnings = append(o.Warnings, "additional app-server endpoints may retain previous credentials; restart them after switching")
	}
	o.Daemon, o.Record, o.Busy, o.Version = switcher.Running, record, busy, rpc.Info.UserAgent
	o.Warnings = append(o.Warnings, "background activity is not completely observable; native shutdown may interrupt it")
	return o, nil
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
func explicitRemote(args []string, socket string) bool {
	for n, arg := range args {
		if arg == "--remote" && n+1 < len(args) && args[n+1] == "unix://"+socket {
			return true
		}
		if arg == "--remote=unix://"+socket {
			return true
		}
	}
	return false
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
