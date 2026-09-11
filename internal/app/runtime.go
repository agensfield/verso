package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agensfield/verso/internal/codex"
	"github.com/agensfield/verso/internal/switcher"
)

type Runtime interface {
	Inspect(context.Context) (codex.Observation, error)
	Stop(context.Context, codex.ProcessRecord) error
	Start(context.Context, string) error
	Verify(context.Context, codex.ProcessRecord, codex.NativeSelection) error
}

type NativeRuntime struct {
	Inspector codex.Inspector
	// Execute is a fixture seam. Production commands get the selected home and
	// explicit startup cwd, with output kept out of error strings.
	Execute func(context.Context, string, string, []string, []string) error
}

func (r *NativeRuntime) Inspect(ctx context.Context) (codex.Observation, error) {
	return r.Inspector.Inspect(ctx)
}
func (r *NativeRuntime) Verify(ctx context.Context, old codex.ProcessRecord, target codex.NativeSelection) error {
	_, err := r.Inspector.VerifyFreshSelection(ctx, old, target)
	return err
}
func (r *NativeRuntime) Stop(ctx context.Context, expected codex.ProcessRecord) error {
	current, err := r.Inspect(ctx)
	if err != nil {
		return err
	}
	if current.Daemon != switcher.Running || current.Record == nil || *current.Record != expected {
		return errors.New("managed daemon changed before stop")
	}
	if len(current.Busy) > 0 {
		return switcher.ErrBusy
	}
	if err := r.command(ctx, "stop", current.Credential.StartupCWD); err != nil {
		return err
	}
	after, err := r.Inspect(ctx)
	if err != nil || after.Daemon != switcher.Stopped {
		return errors.New("managed daemon exit could not be confirmed")
	}
	return nil
}
func (r *NativeRuntime) Start(ctx context.Context, cwd string) error {
	return r.command(ctx, "start", cwd)
}
func (r *NativeRuntime) command(ctx context.Context, action, cwd string) error {
	ctx, cancel := context.WithTimeout(ctx, 80*time.Second)
	defer cancel()
	binary := r.Inspector.Binary
	if binary == "" {
		binary = "codex"
	}
	env := r.Inspector.Env
	if env == nil {
		env = os.Environ()
	}
	var launchEnv []string
	for _, entry := range env {
		if !strings.HasPrefix(entry, "CODEX_HOME=") {
			launchEnv = append(launchEnv, entry)
		}
	}
	launchEnv = append(launchEnv, "CODEX_HOME="+r.Inspector.Home)
	args := []string{"app-server", "daemon", action}
	if r.Execute != nil {
		return r.Execute(ctx, binary, cwd, launchEnv, args)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = launchEnv
	cmd.Dir = cwd
	if err := cmd.Run(); err != nil {
		return errors.New("Codex daemon " + action + " failed")
	}
	return nil
}

var nativeVersion = regexp.MustCompile(`(?:^|\s)(\d+)\.(\d+)\.(\d+)(?:[-+][^\s]+)?(?:$|\s)`)

func SupportedVersion(output string) bool {
	m := nativeVersion.FindStringSubmatch(output)
	if m == nil {
		return false
	}
	major, e1 := strconv.Atoi(m[1])
	minor, e2 := strconv.Atoi(m[2])
	_, e3 := strconv.Atoi(m[3])
	return e1 == nil && e2 == nil && e3 == nil && (major > 0 || minor >= 152)
}
