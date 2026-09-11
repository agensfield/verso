package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAgentSwitchGuardAndNoPipedApproval(t *testing.T) {
	a, _, _ := appFixture(t)
	a.In = strings.NewReader("yes\n")
	if a.requireHuman() == nil {
		t.Fatal("piped approval accepted")
	}
	for _, key := range []string{"CODEX_THREAD_ID", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "CLAUDECODE"} {
		a.Env = []string{key + "=present"}
		if err := a.requireHuman(); err == nil || !strings.Contains(err.Error(), "agents may preview") {
			t.Fatal(err)
		}
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
