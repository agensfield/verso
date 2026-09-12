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

func managedPreferencesPresent(context.Context, CommandRunner) (bool, error) {
	return false, nil
}

func (i Inspector) ownsSocket(_ context.Context, pid int, socket string) (bool, error) {
	raw, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return false, err
	}
	inode, err := listeningSocketInode(raw, socket)
	if err != nil || inode == "" {
		return false, err
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

func listeningSocketInode(raw []byte, socket string) (string, error) {
	inode := ""
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		// Accepted Unix stream sockets can temporarily retain the listener's
		// pathname with a different inode. Only the listening endpoint is the
		// ownership proof: Flags has SO_ACCEPTCON and St is SS_UNCONNECTED.
		if len(fields) < 8 || fields[3] != "00010000" || fields[5] != "01" || strings.Join(fields[7:], " ") != socket {
			continue
		}
		if inode != "" && inode != fields[6] {
			return "", errors.New("multiple socket identities share endpoint path")
		}
		inode = fields[6]
	}
	return inode, nil
}
