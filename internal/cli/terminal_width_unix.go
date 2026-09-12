//go:build linux || darwin

package cli

import "golang.org/x/sys/unix"

func terminalWidth(fd uintptr) int {
	size, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(size.Col)
}
