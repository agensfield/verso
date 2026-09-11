//go:build linux

package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (i Inspector) processCWD(ctx context.Context, pid int) (string, error) {
	raw, err := i.command(ctx, "readlink", filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		return "", errors.New("cannot inspect process cwd")
	}
	return filepath.Clean(string(bytes.TrimSpace(raw))), nil
}

func (i Inspector) ownsSocket(_ context.Context, pid int, socket string) (bool, error) {
	raw, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return false, err
	}
	inode := ""
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || strings.Join(fields[7:], " ") != socket {
			continue
		}
		if inode != "" && inode != fields[6] {
			return false, errors.New("multiple socket identities share endpoint path")
		}
		inode = fields[6]
	}
	if inode == "" {
		return false, nil
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		link, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err == nil && link == "socket:["+inode+"]" {
			return true, nil
		}
	}
	return false, nil
}
