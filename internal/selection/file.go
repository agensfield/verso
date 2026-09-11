// Package selection changes only the native credential document. Runtime and
// effective configuration checks belong to the switch transaction caller.
package selection

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/operation"
)

var ErrChanged = errors.New("native credential identity changed; re-inspect before continuing")

// Read refuses links, oversized files, foreign owners, and exposed credentials.
// Missing auth is a known logged-out state; other modes are not treated as logout.
func Read(home string) ([]byte, accounts.ActiveIdentity, error) {
	f, err := os.OpenFile(filepath.Join(home, "auth.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, accounts.ActiveIdentity{Known: true}, nil
	}
	if err != nil {
		return nil, accounts.ActiveIdentity{}, errors.New("cannot safely read native credentials")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, accounts.ActiveIdentity{}, errors.New("cannot inspect native credentials")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Size() > 1<<20 || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Geteuid()) {
		return nil, accounts.ActiveIdentity{}, errors.New("native credentials must be private, owned, and regular")
	}
	raw := make([]byte, info.Size()+1)
	n, err := f.ReadAt(raw, 0)
	// ReadAt returns EOF for a file shorter than the buffer, which is expected.
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, accounts.ActiveIdentity{}, errors.New("cannot read native credentials")
	}
	if int64(n) > info.Size() {
		return nil, accounts.ActiveIdentity{}, ErrChanged
	}
	raw = raw[:n]
	parsed, err := accounts.ParseNativeAuth(raw)
	if err != nil {
		return nil, accounts.ActiveIdentity{}, err
	}
	return raw, accounts.ActiveIdentity{Known: true, UserID: parsed.UserID, AccountID: parsed.AccountID}, nil
}

func Equal(a, b accounts.ActiveIdentity) bool {
	return Complete(a) && Complete(b) && a.UserID == b.UserID && a.AccountID == b.AccountID
}
func Complete(a accounts.ActiveIdentity) bool {
	return a.Known && ((a.UserID == "") == (a.AccountID == ""))
}

// Install verifies the selected identity immediately before replacement. This
// guards stale Verso plans, but is not a lock against unrelated Codex writers.
func Install(home string, raw []byte, expected accounts.ActiveIdentity) error {
	if _, err := accounts.ParseNativeAuth(raw); err != nil {
		return err
	}
	_, current, err := Read(home)
	if err != nil {
		return err
	}
	if !Equal(current, expected) {
		return ErrChanged
	}
	return operation.AtomicWrite(home, "auth.json", raw)
}

// Clear restores a known previously logged-out state during rollback only.
func Clear(home string, expected accounts.ActiveIdentity) error {
	_, current, err := Read(home)
	if err != nil {
		return err
	}
	if !Equal(current, expected) {
		return ErrChanged
	}
	if current.UserID == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(home, "auth.json")); err != nil {
		return errors.New("cannot restore logged-out native state")
	}
	dir, err := os.Open(home)
	if err != nil {
		return errors.New("cannot sync native credential directory")
	}
	defer dir.Close()
	return dir.Sync()
}
