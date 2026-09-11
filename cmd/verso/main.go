package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/agensfield/verso/internal/cli"
)

var version = "0.1.0-alpha.1-dev"

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve user home")
		os.Exit(1)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	state := os.Getenv("VERSO_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "share", "verso")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	app := cli.App{StateDir: state, CodexHome: codexHome, Binary: "codex", Version: version, Env: os.Environ(), In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
	os.Exit(app.Run(ctx, os.Args[1:]))
}
