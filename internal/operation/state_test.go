package operation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agensfield/verso/internal/switcher"
)

func TestLockSerializesAndCanBeReacquired(t *testing.T) {
	r := filepath.Join(t.TempDir(), "state")
	release, err := Lock(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(r); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock error=%v", err)
	}
	release()
	release, err = Lock(r)
	if err != nil {
		t.Fatal(err)
	}
	release()
	info, err := os.Stat(filepath.Join(r, "operation.lock"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("lock metadata %v %v", info, err)
	}
}
func TestJournalRoundTripAndClear(t *testing.T) {
	r := filepath.Join(t.TempDir(), "state")
	j := Journal{r}
	if cp, err := j.Read(); err != nil || cp != nil {
		t.Fatalf("initial %v %v", cp, err)
	}
	want := switcher.Checkpoint{Version: 1, From: "A", Target: "B", HadDaemon: true, Phase: "activating"}
	if err := j.Write(want); err != nil {
		t.Fatal(err)
	}
	cp, err := j.Read()
	if err != nil || *cp != want {
		t.Fatalf("got %v err=%v", cp, err)
	}
	if err := j.Clear(); err != nil {
		t.Fatal(err)
	}
	if cp, err := j.Read(); err != nil || cp != nil {
		t.Fatalf("final %v %v", cp, err)
	}
}
func TestAtomicReplacementAndRefusalPreserveOldState(t *testing.T) {
	r := filepath.Join(t.TempDir(), "state")
	if err := AtomicWrite(r, "snapshot.json", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(r, "../escape", []byte("new")); err == nil {
		t.Fatal("accepted traversal")
	}
	if err := AtomicWrite(r, "snapshot.json", []byte("new")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(r, "snapshot.json"))
	if err != nil || string(raw) != "new" {
		t.Fatalf("read %q %v", raw, err)
	}
	link := filepath.Join(r, "link")
	if err := os.Symlink(filepath.Join(r, "snapshot.json"), link); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(r, "link", []byte("bad")); err == nil {
		t.Fatal("accepted symlink")
	}
	raw, _ = os.ReadFile(filepath.Join(r, "snapshot.json"))
	if string(raw) != "new" {
		t.Fatal("symlink target mutated")
	}
}
func TestMalformedJournalIsNotCleared(t *testing.T) {
	r := filepath.Join(t.TempDir(), "state")
	j := Journal{r}
	if err := AtomicWrite(r, "switch.json", []byte(`{"version":999,"secret":"not echoed"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Read(); err == nil {
		t.Fatal("accepted malformed journal")
	}
	if err := j.Clear(); err == nil {
		t.Fatal("cleared malformed journal")
	}
	if _, err := os.Stat(filepath.Join(r, "switch.json")); err != nil {
		t.Fatal(err)
	}
}
func TestRejectPublicRootAndSymlinkLock(t *testing.T) {
	r := t.TempDir()
	if err := os.Chmod(r, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(r); err == nil {
		t.Fatal("accepted public state")
	}
	if err := os.Chmod(r, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(r, "operation.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(r); err == nil {
		t.Fatal("accepted symlink lock")
	}
}
