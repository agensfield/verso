package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/agensfield/verso/internal/updater"
)

func TestUpdateDispatchAndHomebrewRefusal(t *testing.T) {
	for _, check := range []bool{true, false} {
		a, out, _ := appFixture(t)
		called := false
		a.UpdateAction = func(_ context.Context, got bool) (updater.Result, error) {
			called = true
			if got != check {
				t.Fatal("wrong update mode")
			}
			return updater.Result{InstallKind: "homebrew", Guidance: "brew upgrade verso"}, errors.New("use brew upgrade verso")
		}
		args := []string{"update", "--json"}
		if check {
			args = append(args, "--check")
		}
		if a.Run(context.Background(), args) == 0 || !called {
			t.Fatal(out.String())
		}
	}
}
