package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBundledGuideNeedsNoRuntimeOrAccountState(t *testing.T) {
	for _, args := range [][]string{{"--skill"}, {"skill"}, {"--skill", "--json"}, {"skill", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			a, out, _ := appFixture(t)
			a.RunCommand = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("guide probed a runtime")
				return nil, nil
			}
			if code := a.Run(context.Background(), args); code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			body := out.String()
			if a.json {
				var r response
				if err := json.Unmarshal(out.Bytes(), &r); err != nil {
					t.Fatal(err)
				}
				if !r.OK || r.Command != "skill" || r.Schema != "verso/v1" {
					t.Fatalf("unexpected envelope: %+v", r)
				}
				body = r.Message
			}
			if strings.TrimSpace(body) != strings.TrimSpace(agentGuide) {
				t.Fatal("did not serve bundled guide")
			}
			for _, path := range []string{a.StateDir, a.CodexHome} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("guide touched %s", path)
				}
			}
		})
	}
}

func TestGuideFlagCannotDispatchAnotherCommand(t *testing.T) {
	for _, args := range [][]string{
		{"--skill", "switch", "work"},
		{"--skill", "update"},
		{"--skill", "--version"},
		{"--skill", "--check"},
		{"--skill", "--allow-no-snapshot"},
		{"skill", "--check"},
		{"skill", "extra"},
	} {
		a, _, _ := appFixture(t)
		a.RunCommand = func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("guide dispatched a runtime command")
			return nil, nil
		}
		if a.Run(context.Background(), args) == 0 {
			t.Fatalf("accepted mixed arguments %v", args)
		}
		if _, err := os.Stat(a.StateDir); !os.IsNotExist(err) {
			t.Fatal("created state")
		}
	}
}
