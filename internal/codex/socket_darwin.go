//go:build darwin

package codex

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

func (i Inspector) processCWD(ctx context.Context, pid int) (string, error) {
	raw, err := i.command(ctx, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	if err != nil {
		return "", errors.New("cannot inspect process cwd")
	}
	matchingPID := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "p") {
			matchingPID = line == "p"+strconv.Itoa(pid)
		} else if matchingPID && strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n"), nil
		}
	}
	return "", errors.New("cannot inspect process cwd")
}

func managedPreferencesPresent(ctx context.Context, run CommandRunner) (bool, error) {
	raw, err := run(ctx, "/usr/bin/defaults", "read", "com.openai.codex")
	if err == nil {
		text := string(raw)
		return strings.Contains(text, "config_toml_base64") || strings.Contains(text, "requirements_toml_base64"), nil
	}
	domains, domainsErr := run(ctx, "/usr/bin/defaults", "domains")
	if domainsErr != nil {
		return false, errors.New("cannot inspect managed preference domains")
	}
	for _, domain := range strings.Split(string(domains), ",") {
		if strings.TrimSpace(domain) == "com.openai.codex" {
			return false, errors.New("cannot inspect Codex managed preference keys")
		}
	}
	return false, nil
}

func (i Inspector) ownsSocket(ctx context.Context, pid int, socket string) (bool, error) {
	raw, err := i.command(ctx, "/usr/sbin/lsof", "-n", "-a", "-p", strconv.Itoa(pid), "-U", "-Fpn")
	if err != nil {
		return false, err
	}
	matchingPID := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "p") {
			matchingPID = line == "p"+strconv.Itoa(pid)
		}
		if matchingPID && line == "n"+socket {
			return true, nil
		}
	}
	return false, nil
}
