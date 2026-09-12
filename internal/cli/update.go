package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/agensfield/verso/internal/updater"
)

func (a *App) updateCommand(ctx context.Context, args []string, check bool) int {
	r := response{Command: "update"}
	if len(args) != 0 {
		return a.finish(r, errors.New("usage: verso update [--check]"))
	}
	options := updater.Options{CurrentVersion: a.Version}
	var result updater.Result
	var err error
	if check {
		a.progress("Checking for updates...")
	} else {
		a.progress("Checking and installing the latest update...")
	}
	if a.UpdateAction != nil {
		result, err = a.UpdateAction(ctx, check)
	} else if check {
		result, err = updater.Check(ctx, options)
	} else {
		result, err = updater.Update(ctx, options)
	}
	r.Update = &result
	r.UpdateInfo = &updateMetadata{Current: result.Current, Latest: result.Latest, Status: result.Status, InstallKind: result.InstallKind, Updated: result.Updated, Guidance: result.Guidance}
	if err == nil {
		r.Message = fmt.Sprintf("Current: %s; latest: %s (%s).", result.Current, result.Latest, result.Status)
		if result.Updated {
			r.Message = "Updated to " + result.Latest + "."
		}
		if result.Guidance != "" {
			r.Message += " " + result.Guidance
		}
	}
	return a.finish(r, err)
}
