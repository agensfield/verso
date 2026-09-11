package cli

import (
	"os"
	"strings"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/presentation"
	"github.com/mattn/go-isatty"
)

func (a *App) renderAccounts(r response) error {
	return presentation.WriteAccounts(a.Out, r.Accounts, r.Quotas, r.Active, r.Cached, a.accountColor(), time.Now())
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
	env := a.Env
	if env == nil {
		env = os.Environ()
	}
	return accountColorEnabled(isatty.IsTerminal(out.Fd()), env)
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
