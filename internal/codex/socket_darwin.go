//go:build darwin

package codex

import (
	"context"
	"strconv"
	"strings"
)

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
