package cli

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/agensfield/verso/internal/accounts"
	application "github.com/agensfield/verso/internal/app"
	"github.com/agensfield/verso/internal/operation"
	"github.com/agensfield/verso/internal/selection"
)

func (a *App) accountMutation(ctx context.Context, command string, args []string) int {
	r := response{Command: command}
	if (command == "import" && len(args) > 1) || (command == "remove" && len(args) != 1) {
		return a.finish(r, errors.New("usage: verso import [alias] | verso remove <account>"))
	}
	if !filepath.IsAbs(a.StateDir) || !filepath.IsAbs(a.CodexHome) {
		return a.finish(r, errors.New("state and Codex home paths must be absolute"))
	}
	release, err := operation.Lock(a.StateDir)
	if err != nil {
		return a.finish(r, err)
	}
	defer release()
	inspector, err := a.inspector()
	if err != nil {
		return a.finish(r, err)
	}
	observed, err := inspector.InspectSelection(ctx)
	if err != nil {
		return a.finish(r, err)
	}
	if !application.FileSelectionAllowed(observed) {
		return a.finish(r, errors.New("native credential mode is unproven: "+observed.Credential.Reason))
	}
	raw, selected, err := selection.Read(a.CodexHome)
	if err != nil {
		return a.finish(r, err)
	}
	if !selection.Equal(selected, observed.SelectedFile) {
		return a.finish(r, selection.ErrChanged)
	}
	store, err := accounts.Open(filepath.Join(a.StateDir, "accounts"))
	if err != nil {
		return a.finish(r, err)
	}
	if command == "remove" {
		target, err := store.Find(args[0])
		if err != nil {
			return a.finish(r, err)
		}
		if err = store.Remove(target.ID, selected); err != nil {
			return a.finish(r, err)
		}
		r.Target = &target
		r.Message = "Saved account removed."
	} else {
		if selected.UserID == "" {
			return a.finish(r, errors.New("no native ChatGPT login to import; run verso add"))
		}
		parsed, err := accounts.ParseNativeAuth(raw)
		if err != nil {
			return a.finish(r, err)
		}
		alias := ""
		if len(args) == 1 {
			alias = args[0]
		}
		target, err := store.Save(parsed, alias)
		if err != nil {
			return a.finish(r, err)
		}
		r.Target = &target
		r.Message = "Current login saved."
	}
	return a.finish(r, nil)
}
