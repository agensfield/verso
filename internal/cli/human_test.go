package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestAgentSwitchGuardAndNoPipedApproval(t *testing.T) {
	a, _, _ := appFixture(t)
	a.In = strings.NewReader("yes\n")
	if a.requireHuman() == nil {
		t.Fatal("piped approval accepted")
	}
	for _, key := range []string{"CODEX_THREAD_ID", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "CLAUDECODE"} {
		a.Env = []string{key + "=present"}
		if err := a.requireHuman(); err == nil || !strings.Contains(err.Error(), "run this switch directly") {
			t.Fatal(err)
		}
	}
}

func TestAccountNumberRetriesAndCancelsWithContext(t *testing.T) {
	var out bytes.Buffer
	n, err := readAccountNumber(context.Background(), strings.NewReader("9\n2\n"), &out, 2)
	if err != nil || n != 2 || !strings.Contains(out.String(), "Choose a number from 1 to 2") {
		t.Fatalf("selection = %d, %v; output %q", n, err, out.String())
	}

	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := readAccountNumber(ctx, reader, io.Discard, 2)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("account picker ignored cancellation")
	}
}

func TestAccountNumberLeavesConfirmationInputUnread(t *testing.T) {
	in := strings.NewReader("1\ny\n")
	if n, err := readAccountNumber(context.Background(), in, io.Discard, 2); err != nil || n != 1 {
		t.Fatalf("selection = %d, %v", n, err)
	}
	if err := confirm(context.Background(), in, io.Discard, "Switch?"); err != nil {
		t.Fatalf("picker consumed confirmation input: %v", err)
	}
}

func TestConfirmationLeavesLaterPromptInputUnread(t *testing.T) {
	in := strings.NewReader("yes\ny\n")
	if err := confirm(context.Background(), in, io.Discard, "Reauthenticate?"); err != nil {
		t.Fatal(err)
	}
	if err := confirm(context.Background(), in, io.Discard, "Switch?"); err != nil {
		t.Fatalf("first confirmation consumed the second: %v", err)
	}
}
func TestConfirmationNeverDefaultsToApproval(t *testing.T) {
	for _, answer := range []string{"", "\n", "no\n", "maybe\n"} {
		if err := confirm(context.Background(), strings.NewReader(answer), new(bytes.Buffer), "Switch?"); err == nil {
			t.Fatalf("accepted %q", answer)
		}
	}
	if err := confirm(context.Background(), strings.NewReader("y\n"), new(bytes.Buffer), "Switch?"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Even an available yes must not override a context already cancelled.
	if err := confirm(ctx, strings.NewReader("no\n"), new(bytes.Buffer), "Switch?"); err == nil {
		t.Fatal("cancel accepted")
	}
}
