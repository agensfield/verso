package presentation

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/quota"
)

func ptr[T any](value T) *T { return &value }

func TestWriteAccountsSyntheticPreview(t *testing.T) {
	now := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	resetSoon := 2*time.Hour + 15*time.Minute
	fiveHours := 5 * time.Hour
	week := 7 * 24 * time.Hour
	weeklyReset := time.Date(2026, time.September, 15, 10, 0, 0, 0, time.UTC)
	saved := []accounts.Account{
		{ID: "hidden-personal-id", Alias: "personal", Email: "arda@example.com", UserID: "hidden-user", AccountID: "hidden-workspace"},
		{ID: "hidden-work-id", Alias: "work", Email: "work@example.com", UserID: "hidden-user-2", AccountID: "hidden-workspace-2"},
	}
	entries := map[string]quota.Entry{
		"hidden-personal-id": {
			Quota: &auth.Quota{
				Plan:      ptr("plus"),
				Primary:   &auth.Window{UsedPercent: ptr(28.0), Window: &fiveHours, ResetAfter: &resetSoon},
				Secondary: &auth.Window{UsedPercent: ptr(55.0), Window: &week, ResetsAt: &weeklyReset},
			},
			CheckedAt: now.Add(-20 * time.Second),
		},
		"hidden-work-id": {
			Quota:     &auth.Quota{Plan: ptr("team"), Primary: &auth.Window{UsedPercent: ptr(94.0)}},
			CheckedAt: now.Add(-9 * time.Minute),
			Stale:     true,
		},
	}

	var out bytes.Buffer
	if err := WriteAccounts(&out, saved, entries, "hidden-personal-id", true, false, now); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Log("synthetic account preview:\n" + got)
	for _, want := range []string{
		"personal  active · plus",
		"arda@example.com",
		"5h      ",
		"72% left · resets in 2h 15m",
		"weekly",
		"45% left · resets Tue 10:00",
		"work  team",
		"quota 1",
		"6% left",
		"stale · cached · checked 9m ago",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	for _, hidden := range []string{"hidden-personal-id", "hidden-work-id", "hidden-user", "hidden-workspace"} {
		if strings.Contains(got, hidden) {
			t.Errorf("output exposed identifier %q:\n%s", hidden, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("plain output contains ANSI: %q", got)
	}
}

func TestWriteAccountsDoesNotManufactureAbsentUsage(t *testing.T) {
	account := accounts.Account{ID: "secret", Alias: "unknown"}
	entries := map[string]quota.Entry{
		"secret": {Quota: &auth.Quota{Primary: &auth.Window{}}},
	}
	var out bytes.Buffer
	if err := WriteAccounts(&out, []accounts.Account{account}, entries, "", false, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "usage unknown") {
		t.Fatalf("absent usage is not honest: %q", got)
	}
	if strings.Contains(got, "% left") || strings.ContainsAny(got, "▰▱") {
		t.Fatalf("absent usage grew a phantom percentage or bar: %q", got)
	}
}

func TestWriteAccountsShowsLoginAndAuthoritativeExhaustion(t *testing.T) {
	account := accounts.Account{ID: "secret", Alias: "work"}
	entries := map[string]quota.Entry{
		"secret": {
			Quota:         &auth.Quota{Exhausted: ptr(true), Primary: &auth.Window{UsedPercent: ptr(30.0)}},
			LoginRequired: true,
			Stale:         true,
		},
	}
	var out bytes.Buffer
	if err := WriteAccounts(&out, []accounts.Account{account}, entries, "", false, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"70% left", "login needed", "limit reached", "stale"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %q", want, got)
		}
	}
}

func TestWriteAccountsSanitizesExternalLabels(t *testing.T) {
	account := accounts.Account{
		ID:    "secret-id",
		Alias: "per\x1b[31mson\u202eal\nadmin",
		Email: "mail\u2066@example.com",
	}
	entry := quota.Entry{Quota: &auth.Quota{Plan: ptr("pl\x00us\u200f")}}
	var out bytes.Buffer
	if err := WriteAccounts(&out, []accounts.Account{account}, map[string]quota.Entry{"secret-id": entry}, "", false, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, unsafe := range []string{"\x1b", "\r", "\u202e", "\u2066", "\u200f", "\x00"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("unsafe label bytes reached output: %q", got)
		}
	}
	for _, want := range []string{"per [31mson al admin", "mail @example.com", "pl us"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitized output missing %q: %q", want, got)
		}
	}
}

func TestWriteAccountsColorIsOptIn(t *testing.T) {
	account := accounts.Account{ID: "secret", Alias: "personal"}
	entry := quota.Entry{Quota: &auth.Quota{Primary: &auth.Window{UsedPercent: ptr(99.6)}}}
	var out bytes.Buffer
	if err := WriteAccounts(&out, []accounts.Account{account}, map[string]quota.Entry{"secret": entry}, "secret", false, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[31;1m <1% left\x1b[0m") || !strings.Contains(out.String(), "\x1b[36mactive") || strings.Contains(out.String(), "0% left") {
		t.Fatalf("expected red sub-1%% quota and active color: %q", out.String())
	}
}

func TestWriteAccountsAgesRelativeResetAndPrefersAbsoluteReset(t *testing.T) {
	now := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	resetAfter := 2 * time.Hour
	absolute := now.Add(3 * time.Hour)
	entries := map[string]quota.Entry{
		"relative": {
			Quota:     &auth.Quota{Primary: &auth.Window{UsedPercent: ptr(50.0), ResetAfter: &resetAfter}},
			CheckedAt: now.Add(-45 * time.Minute),
		},
		"absolute": {
			Quota: &auth.Quota{
				ObservedAt: now.Add(-45 * time.Minute),
				Primary:    &auth.Window{UsedPercent: ptr(50.0), ResetAfter: &resetAfter, ResetsAt: &absolute},
			},
		},
	}
	var out bytes.Buffer
	if err := WriteAccounts(&out, []accounts.Account{{ID: "relative", Alias: "relative"}, {ID: "absolute", Alias: "absolute"}}, entries, "", true, false, now); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "resets in 1h 15m") {
		t.Fatalf("relative reset did not age from CheckedAt: %q", got)
	}
	if !strings.Contains(got, "resets Fri 11:00") || strings.Contains(got, "resets in 2h") {
		t.Fatalf("absolute reset was not preferred: %q", got)
	}
}

func TestWriteAccountsUsesNeutralLabelWithoutWindowDuration(t *testing.T) {
	entry := quota.Entry{Quota: &auth.Quota{
		Primary:   &auth.Window{UsedPercent: ptr(10.0)},
		Secondary: &auth.Window{UsedPercent: ptr(20.0)},
	}}
	var out bytes.Buffer
	if err := WriteAccounts(&out, []accounts.Account{{ID: "secret", Alias: "account"}}, map[string]quota.Entry{"secret": entry}, "", false, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "quota 1") || !strings.Contains(got, "quota 2") || strings.Contains(got, "weekly") {
		t.Fatalf("missing durations received invented labels: %q", got)
	}
}

func TestWriteAccountsReturnsWriterError(t *testing.T) {
	want := errors.New("write failed")
	err := WriteAccounts(errorWriter{want}, []accounts.Account{{Alias: "personal"}}, nil, "", false, false, time.Now())
	if !errors.Is(err, want) {
		t.Fatalf("WriteAccounts error = %v, want %v", err, want)
	}
}

func TestNameFallbacksAndSanitizes(t *testing.T) {
	if got := Name(accounts.Account{Alias: "\u202e\n", Email: "a@example.com"}); got != "a@example.com" {
		t.Fatalf("Name() = %q", got)
	}
	if got := Name(accounts.Account{}); got != "account" {
		t.Fatalf("Name() = %q", got)
	}
}

func TestWriteAccountsDoesNotRepeatEmailAlias(t *testing.T) {
	var out bytes.Buffer
	account := accounts.Account{Alias: "same@example.com", Email: "SAME@example.com"}
	if err := WriteAccounts(&out, []accounts.Account{account}, nil, "", false, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.ToLower(out.String()), "same@example.com") != 1 {
		t.Fatalf("email alias repeated: %q", out.String())
	}
}

func TestAccountCardsAdaptToNarrowAndPlainTerminals(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	entry := quota.Entry{
		Quota:   &auth.Quota{Primary: &auth.Window{UsedPercent: ptr(25.0), ResetsAt: &reset}},
		Warning: "usage refresh failed; cached observation retained",
	}
	var out bytes.Buffer
	err := WriteAccountCards(&out, []accounts.Account{{ID: "secret", Alias: strings.Repeat("界", 30), Email: "wide@example.test"}}, map[string]quota.Entry{"secret": entry}, "secret", false, false, now, AccountOptions{Width: 40, Plain: true, Numbered: true})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "1  ") || strings.ContainsAny(got, "▰▱") || !strings.Contains(got, "resets Sat 14:00") || !strings.Contains(got, "warning:") {
		t.Fatalf("narrow plain card lost semantics: %q", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if width := cellWidth(stripANSI(line)); width > 40 {
			t.Errorf("line is %d cells: %q", width, line)
		}
	}
}

func TestAccountCardsRenderPastAndFutureTimesHonestly(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	past := now.Add(-7 * 24 * time.Hour)
	entry := quota.Entry{Quota: &auth.Quota{Primary: &auth.Window{UsedPercent: ptr(50.0), ResetsAt: &past}}, CheckedAt: now.Add(time.Minute), Stale: true}
	var out bytes.Buffer
	if err := WriteAccounts(&out, []accounts.Account{{ID: "secret", Alias: "work"}}, map[string]quota.Entry{"secret": entry}, "", true, false, now); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "reset due Sep 5 12:00") || !strings.Contains(got, "checked in the future") || strings.Contains(got, "just now") {
		t.Fatalf("dishonest time labels: %q", got)
	}
}

func TestAccountCardsBudgetHeadingDetailsAndResetContinuations(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	past := now.Add(-7 * 24 * time.Hour)
	plan := strings.Repeat("界", 20)
	entry := quota.Entry{Quota: &auth.Quota{Plan: &plan, Primary: &auth.Window{UsedPercent: ptr(50.0), ResetsAt: &past}}}
	var out bytes.Buffer
	if err := WriteAccountCards(&out, []accounts.Account{{ID: "secret", Alias: strings.Repeat("界", 20)}}, map[string]quota.Entry{"secret": entry}, "secret", false, false, now, AccountOptions{Width: 20, Plain: true, Numbered: true}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if width := cellWidth(stripANSI(line)); width > 20 {
			t.Errorf("line is %d cells: %q\n%s", width, line, out.String())
		}
	}
	for _, want := range []string{"active", "50% left", "reset due"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %q", want, out.String())
		}
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }
