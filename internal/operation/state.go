// Package operation persists non-secret switch progress and serializes Verso writers.
package operation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/agensfield/verso/internal/switcher"
)

var ErrLocked = errors.New("another Verso operation is active; no switch performed")

func privateRoot(root string) error {
	if !filepath.IsAbs(root) {
		return errors.New("Verso state path must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("Verso state directory must be private, owned, and not a symlink")
	}
	return nil
}

// Lock is nonblocking. The file remains in place so all writers lock one inode.
// Do not delete lock files as stale-lock recovery; flock is released on process exit.
func Lock(root string) (func(), error) {
	if err := privateRoot(root); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, "operation.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("cannot open Verso operation lock")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return nil, errors.New("unsafe operation lock file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, errors.New("cannot acquire Verso operation lock")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

type Journal struct{ Root string }

func (j Journal) Read() (*switcher.Checkpoint, error) {
	rootInfo, rootErr := os.Lstat(j.Root)
	if errors.Is(rootErr, os.ErrNotExist) {
		return nil, nil
	}
	if rootErr != nil {
		return nil, errors.New("cannot inspect switch journal directory")
	}
	rootStat, rootOK := rootInfo.Sys().(*syscall.Stat_t)
	if !rootInfo.IsDir() || !rootOK || rootStat.Uid != uint32(os.Geteuid()) || rootInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe switch journal directory")
	}
	// O_NONBLOCK prevents a crafted FIFO or device from stalling recovery before
	// descriptor validation can reject it. It has no effect on regular files.
	f, err := os.OpenFile(filepath.Join(j.Root, "switch.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot read switch journal safely")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return nil, errors.New("invalid or unprotected switch journal")
	}
	var cp switcher.Checkpoint
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(&cp) != nil || cp.Version != 1 || cp.Target == "" {
		return nil, errors.New("invalid or unsupported switch journal; manual recovery required")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, errors.New("invalid trailing switch journal data")
	}
	switch cp.Phase {
	case "prepared", "stopping", "activating", "starting", "committed", "rolling_back", "rolled_back":
	default:
		return nil, errors.New("unknown switch journal phase; manual recovery required")
	}
	return &cp, nil
}

func (j Journal) Write(cp switcher.Checkpoint) error {
	if cp.Version != 1 || cp.Target == "" {
		return errors.New("invalid checkpoint")
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(j.Root, "switch.json", append(data, '\n'))
}

func (j Journal) Clear() error {
	if _, err := j.Read(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(j.Root, "switch.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove switch journal")
	}
	return syncDir(j.Root)
}

// AtomicWrite replaces one private state artifact; callers hold the operation lock.
func AtomicWrite(root, name string, data []byte) error {
	if filepath.Base(name) != name || name == "." || name == "" {
		return errors.New("invalid state filename")
	}
	if err := privateRoot(root); err != nil {
		return err
	}
	dest := filepath.Join(root, name)
	if info, err := os.Lstat(dest); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("unsafe existing state file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect state destination")
	}
	f, err := os.CreateTemp(root, "."+name+"-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return errors.New("state file write failed")
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return errors.New("state file sync failed")
	}
	if err = f.Close(); err != nil {
		return errors.New("state file close failed")
	}
	if err = os.Rename(temp, dest); err != nil {
		return errors.New("state file replacement failed")
	}
	return syncDir(root)
}

func syncDir(root string) error {
	d, err := os.Open(root)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("state directory sync failed: %w", err)
	}
	return nil
}
