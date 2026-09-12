package cli

import (
	"errors"
	"fmt"
	"strings"
)

var commandHelp = map[string]string{
	"list":     "verso list [account] [--cached] [--json]\n\nRefresh saved account metadata and usage. Inactive credentials may refresh when needed.\nUse --cached for saved metadata and quota only, with no network, RPC, or writes.\n",
	"switch":   "verso switch [account] [--allow-exhausted] [--allow-no-snapshot]\n\nChoose an account, review the checks, and confirm the switch.\nOmit the account to open the picker.\n",
	"add":      "verso add [alias]\n\nSign in with a device code and save the account. Your selected account stays put.\n",
	"import":   "verso import [alias] [--json]\n\nSave your current Codex login. The alias defaults to your email.\n",
	"remove":   "verso remove <account> [--json]\n\nRemove a saved account. The selected account cannot be removed.\n",
	"preview":  "verso preview <account> [--allow-exhausted] [--allow-no-snapshot] [--json]\n\nCheck a switch without refreshing credentials or changing accounts.\n--allow-exhausted permits a target known to be exhausted.\n--allow-no-snapshot permits switching when Herdr recovery capture fails.\n",
	"status":   "verso status [--json]\n\nShow the selected login and Codex runtime status.\n",
	"recovery": "verso recovery [--json]\n\nRead unfinished-switch details and the latest Herdr snapshot.\nNothing is restored automatically.\n",
	"update":   "verso update [--check] [--json]\n\nInstall the latest version, or check without installing.\nFor Homebrew installations, use brew upgrade verso.\n",
	"version":  "verso version\n\nPrint the installed version.\n",
	"licenses": "verso licenses\n\nPrint the license and dependency notices.\n",
	"skill":    "verso --skill [--json]\n\nRead the bundled agent guide. Also available as verso skill.\n",
	"schema":   "verso schema [--json]\n\nPrint offline verso/v1 command and effect metadata.\n",
	"options":  "Global options\n\n  --json                Print results as JSON\n  --skill               Print the bundled agent guide\n  --state-dir PATH      Verso data directory\n  --codex-home PATH     Codex home (CODEX_HOME or ~/.codex)\n  --codex-bin PATH      Codex executable\n\nDiscovery: verso help options | verso schema --json\nAccount names may be aliases, emails, or saved IDs. Use list --cached --json for IDs.\nCommand-only flags are documented under verso help <command>.\n",
}

func (a *App) printHelp(topic string) int {
	text := usage
	if topic != "" {
		var ok bool
		text, ok = commandHelp[topic]
		if !ok {
			return a.finish(response{Command: "help"}, errors.New("unknown help topic; use verso --help"))
		}
	}
	if a.json {
		return a.finish(response{Command: "help", Message: text}, nil)
	}
	if a.accountColor() {
		if first, rest, found := strings.Cut(text, "\n"); found {
			text = a.humanHeading(first) + "\n" + rest
		}
	}
	_, err := fmt.Fprint(a.Out, text)
	if err != nil {
		return 1
	}
	return 0
}

// flagsFirst leaves command arguments after its explicit separator.
func helpTopic(ordered []string) string {
	for n, arg := range ordered {
		if arg != "--" || n+1 >= len(ordered) {
			continue
		}
		if ordered[n+1] == "help" && n+2 < len(ordered) {
			return ordered[n+2]
		}
		return ordered[n+1]
	}
	return ""
}
