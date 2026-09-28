package codex

import (
	"errors"
	"os"
	"path/filepath"
)

// resolveManagedSocket keeps the stable control directory as the trust
// boundary while allowing Codex to rotate the final socket through a symlink.
// Codex 0.157 uses this layout for its managed daemon.
func resolveManagedSocket(socket string) (string, error) {
	abs, err := filepath.Abs(socket)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", err
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("managed socket control directory is redirected")
	}
	return filepath.EvalSymlinks(abs)
}
