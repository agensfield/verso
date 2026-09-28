package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveManagedSocketFollowsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	control := filepath.Join(root, "app-server-control")
	if err := os.Mkdir(control, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "rotated.sock")
	if err := os.WriteFile(target, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(control, "app-server-control.sock")
	if err := os.Symlink(target, stable); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveManagedSocket(stable)
	want, _ := filepath.EvalSymlinks(target)
	if err != nil || resolved != want {
		t.Fatalf("resolved=%q err=%v", resolved, err)
	}
}

func TestResolveManagedSocketRejectsRedirectedControlDirectory(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	redirected := filepath.Join(root, "app-server-control")
	if err := os.Symlink(actual, redirected); err != nil {
		t.Fatal(err)
	}
	_, err := resolveManagedSocket(filepath.Join(redirected, "app-server-control.sock"))
	if err == nil || !strings.Contains(err.Error(), "control directory is redirected") {
		t.Fatalf("err=%v", err)
	}
}
