package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/presentation"
	"github.com/mattn/go-isatty"
)

func (a *App) renderAccounts(r response) error {
	return a.renderAccountCards(r, false)
}

func (a *App) renderAccountCards(r response, numbered bool) error {
	return presentation.WriteAccountCards(a.Out, r.Accounts, r.Quotas, r.Active, r.Cached, a.accountColor(), time.Now(), presentation.AccountOptions{
		Width: a.accountWidth(), Plain: a.plainTerminal(), Numbered: numbered,
	})
}

func (a *App) accountWidth() int {
	if out, ok := a.Out.(*os.File); ok {
		if width := terminalWidth(out.Fd()); width >= 20 && width <= 500 {
			return width
		}
	}
	for _, entry := range a.environment() {
		key, value, found := strings.Cut(entry, "=")
		if !found || key != "COLUMNS" {
			continue
		}
		width, err := strconv.Atoi(value)
		if err == nil && width >= 20 && width <= 500 {
			return width
		}
	}
	return 80
}

func (a *App) plainTerminal() bool {
	for _, entry := range a.environment() {
		key, value, found := strings.Cut(entry, "=")
		if found && key == "TERM" && strings.EqualFold(value, "dumb") {
			return true
		}
	}
	return false
}

func (a *App) environment() []string {
	if a.Env != nil {
		return a.Env
	}
	return os.Environ()
}

func (a *App) progress(format string, args ...any) {
	if a.json {
		return
	}
	errOut, ok := a.Err.(*os.File)
	if !ok || !isatty.IsTerminal(errOut.Fd()) {
		return
	}
	_, _ = fmt.Fprintf(a.Err, format+"\n", args...)
}

func accountChoiceName(account accounts.Account) string {
	return presentation.Name(account)
}

func (a *App) humanHeading(text string) string {
	if !a.accountColor() {
		return text
	}
	return "\x1b[1;36m" + text + "\x1b[0m"
}

func selectedAccountName(r response) string {
	if r.Runtime == nil {
		return "unknown"
	}
	selected := r.Runtime.SelectedFile
	if selected.Known {
		if selected.UserID == "" && selected.AccountID == "" {
			return "not signed in"
		}
		for _, account := range r.Accounts {
			if account.UserID == selected.UserID && account.AccountID == selected.AccountID {
				return presentation.Name(account)
			}
		}
	}
	if r.Runtime.SelectedEmail == "" {
		return "unknown"
	}
	return presentation.Name(accounts.Account{Email: r.Runtime.SelectedEmail})
}

func (a *App) accountColor() bool {
	if a.json {
		return false
	}
	out, ok := a.Out.(*os.File)
	if !ok {
		return false
	}
	return accountColorEnabled(isatty.IsTerminal(out.Fd()), a.environment())
}

func accountColorEnabled(terminal bool, env []string) bool {
	if !terminal {
		return false
	}
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if key == "NO_COLOR" {
			return false
		}
		if found && key == "TERM" && strings.EqualFold(value, "dumb") {
			return false
		}
	}
	return true
}
