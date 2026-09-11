package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
)

func agentEnvironment(env []string) bool {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		switch key {
		case "CODEX_THREAD_ID", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "CLAUDECODE":
			return true
		}
	}
	return false
}

func (a *App) requireHuman() error {
	if agentEnvironment(a.Env) {
		return errors.New("agents may preview only; run the switch directly in your terminal")
	}
	input, inOK := a.In.(*os.File)
	output, outOK := a.Out.(*os.File)
	if a.json || !inOK || !outOK || !isatty.IsTerminal(input.Fd()) || !isatty.IsTerminal(output.Fd()) {
		return errors.New("switch execution requires an interactive human terminal; use preview for automation")
	}
	return nil
}

// confirm has no implicit approval on EOF, timeout, cancellation, or empty input.
func confirm(ctx context.Context, in io.Reader, out io.Writer, prompt string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprint(out, prompt+" [y/N] "); err != nil {
		return err
	}
	answer := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(io.LimitReader(in, 4096))
		accepted := false
		if scanner.Scan() {
			line := strings.ToLower(strings.TrimSpace(scanner.Text()))
			accepted = line == "y" || line == "yes"
		}
		answer <- accepted
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case yes := <-answer:
		if err := ctx.Err(); err != nil {
			return err
		}
		if !yes {
			return errors.New("cancelled; no switch performed")
		}
		return nil
	}
}
