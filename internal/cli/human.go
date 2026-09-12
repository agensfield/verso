package cli

import (
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
		return errors.New("run this switch directly in your terminal; use verso preview to inspect it here")
	}
	input, inOK := a.In.(*os.File)
	output, outOK := a.Out.(*os.File)
	if a.json || !inOK || !outOK || !isatty.IsTerminal(input.Fd()) || !isatty.IsTerminal(output.Fd()) {
		return errors.New("run this switch in an interactive terminal; use verso preview for a read-only check")
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
	line, err := readLine(ctx, in, 4096)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("cancelled; no switch performed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		return errors.New("cancelled; no switch performed")
	}
	return nil
}
