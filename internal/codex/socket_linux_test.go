//go:build linux

package codex

import "testing"

func TestListeningSocketInodeIgnoresAcceptedPeersWithSamePath(t *testing.T) {
	const table = `Num RefCount Protocol Flags Type St Inode Path
0001: 00000002 00000000 00010000 0001 01 101 /tmp/control.sock
0002: 00000003 00000000 00000000 0001 03 202 /tmp/control.sock
`
	inode, err := listeningSocketInode([]byte(table), "/tmp/control.sock")
	if err != nil || inode != "101" {
		t.Fatalf("inode=%q err=%v", inode, err)
	}
}

func TestListeningSocketInodeRejectsMultipleListeners(t *testing.T) {
	const table = `0001: 00000002 00000000 00010000 0001 01 101 /tmp/control.sock
0002: 00000002 00000000 00010000 0001 01 202 /tmp/control.sock
`
	if _, err := listeningSocketInode([]byte(table), "/tmp/control.sock"); err == nil {
		t.Fatal("multiple listening socket identities accepted")
	}
}
