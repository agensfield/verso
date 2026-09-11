package cli

import "testing"

func TestAccountColorRequiresCapableTTY(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal bool
		env      []string
		want     bool
	}{
		{name: "terminal", terminal: true, env: []string{"TERM=xterm-256color"}, want: true},
		{name: "pipe", env: []string{"TERM=xterm-256color"}},
		{name: "no color", terminal: true, env: []string{"NO_COLOR="}},
		{name: "dumb terminal", terminal: true, env: []string{"TERM=dumb"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := accountColorEnabled(test.terminal, test.env); got != test.want {
				t.Fatalf("accountColorEnabled() = %v, want %v", got, test.want)
			}
		})
	}
}
