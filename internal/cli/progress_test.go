package cli

import (
	"strings"
	"testing"
)

func TestFitProgressMessageRespectsWidth(t *testing.T) {
	for _, test := range []struct {
		message string
		width   int
		want    string
	}{
		{message: "short", width: 8, want: "short"},
		{message: "too long", width: 5, want: "too …"},
		{message: "wide enough", width: 1, want: "…"},
		{message: "界界界", width: 5, want: "界界…"},
		{message: "none", width: 0, want: ""},
	} {
		if got := fitProgressMessage(test.message, test.width); got != test.want {
			t.Errorf("fitProgressMessage(%q, %d) = %q, want %q", test.message, test.width, got, test.want)
		}
	}
}

func TestCleanProgressMessageRemovesTerminalControls(t *testing.T) {
	got := cleanProgressMessage("  checking\nunsafe\x1b[31m\t now  ")
	if got != "checkingunsafe[31m  now" || strings.ContainsAny(got, "\r\n\x1b") {
		t.Fatalf("unsafe progress message %q", got)
	}
}
