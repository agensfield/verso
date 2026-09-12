//go:build !linux && !darwin

package cli

func terminalWidth(uintptr) int { return 0 }
