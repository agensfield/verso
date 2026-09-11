package herdr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixture = `{"result":{"snapshot":{"version":"test","workspaces":[{"workspace_id":"w1","label":"work"}],"tabs":[{"tab_id":"w1:t1","workspace_id":"w1","label":"orchestrator"}],"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1","cwd":"/synthetic","terminal_title":"SECRET-TITLE","environment":{"TOKEN":"SECRET-ENV"}}],"agents":[{"name":"verso","agent":"codex","pane_id":"w1:p1","agent_session":{"kind":"id","value":"synthetic-thread-id","source":"herdr:codex"}}],"layouts":[],"transcript":"SECRET-TRANSCRIPT"}}}`

func TestSnapshotWhitelistRotationAndFailedCapturePreservesPrevious(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(123, 0)
	if err := Capture(root, []byte(fixture), now); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "herdr-snapshot.json")
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET") || !strings.Contains(string(raw), "synthetic-thread-id") {
		t.Fatal("incorrect metadata whitelist")
	}
	info, _ := os.Stat(name)
	if info.Mode().Perm() != 0600 {
		t.Fatal("snapshot permissions")
	}
	if err := Capture(root, []byte(`{"result":{}}`), now); err == nil {
		t.Fatal("invalid accepted")
	}
	previous, _ := os.ReadFile(name)
	if string(previous) != string(raw) {
		t.Fatal("failed capture replaced checkpoint")
	}
	if err := Capture(root, []byte(fixture), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	next, _ := os.ReadFile(name)
	if string(next) == string(raw) {
		t.Fatal("did not rotate")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("retained snapshot archive")
	}
}
func TestBrokenLocatorRefusesCapture(t *testing.T) {
	root := t.TempDir()
	broken := strings.Replace(fixture, `"pane_id":"w1:p1","agent_session"`, `"pane_id":"missing","agent_session"`, 1)
	if err := Capture(root, []byte(broken), time.Now()); err == nil {
		t.Fatal("dangling agent accepted")
	}
}
