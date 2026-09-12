// Package presentation renders human-facing account information.
package presentation

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/agensfield/verso/internal/accounts"
	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/quota"
)

const barWidth = 10

// AccountOptions controls terminal-only layout. Zero values preserve the
// ordinary unnumbered 80-column renderer.
type AccountOptions struct {
	Width    int
	Plain    bool
	Numbered bool
}

type palette struct {
	enabled bool
}

func (p palette) ansi(code, text string) string {
	if !p.enabled || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (p palette) bold(text string) string  { return p.ansi("1", text) }
func (p palette) quiet(text string) string { return p.ansi("2", text) }

// Name returns the safe human label used for an account in lists and pickers.
func Name(account accounts.Account) string {
	if alias := safeLabel(account.Alias); alias != "" {
		return truncateLabel(alias, 48)
	}
	if email := safeLabel(account.Email); email != "" {
		return truncateLabel(email, 48)
	}
	return "account"
}

// WriteAccounts writes compact human-facing account cards. Account and
// workspace identifiers are used for lookup only and are never rendered.
func WriteAccounts(out io.Writer, saved []accounts.Account, entries map[string]quota.Entry, activeID string, cached bool, color bool, now time.Time) error {
	return WriteAccountCards(out, saved, entries, activeID, cached, color, now, AccountOptions{})
}

// WriteAccountCards writes account cards with optional terminal layout hints.
func WriteAccountCards(out io.Writer, saved []accounts.Account, entries map[string]quota.Entry, activeID string, cached bool, color bool, now time.Time, options AccountOptions) error {
	var b strings.Builder
	p := palette{enabled: color}
	if options.Width <= 0 {
		options.Width = 80
	}
	if len(saved) == 0 {
		b.WriteString("no saved accounts\n")
		return writeString(out, b.String())
	}

	for i, account := range saved {
		if i > 0 {
			b.WriteByte('\n')
		}
		entry, found := entries[account.ID]
		number := 0
		if options.Numbered {
			number = i + 1
		}
		writeAccount(&b, p, account, entry, found, account.ID == activeID, cached, now, number, options)
	}
	return writeString(out, b.String())
}

func writeString(out io.Writer, value string) error {
	written, err := io.WriteString(out, value)
	if err == nil && written != len(value) {
		return io.ErrShortWrite
	}
	return err
}

func writeAccount(b *strings.Builder, p palette, account accounts.Account, entry quota.Entry, found, active, cached bool, now time.Time, number int, options AccountOptions) {
	prefix := ""
	if number > 0 {
		prefix = fmt.Sprintf("%d  ", number)
	}
	var details []string
	var plainDetails []string
	if active {
		details = append(details, p.ansi("36", "active"))
		plainDetails = append(plainDetails, "active")
	}
	if entry.Quota != nil && entry.Quota.Plan != nil {
		if plan := safeLabel(*entry.Quota.Plan); plan != "" {
			plan = truncateCells(plan, 20)
			details = append(details, p.quiet(plan))
			plainDetails = append(plainDetails, plan)
		}
	}
	detailText := ""
	if len(details) > 0 {
		detailText = "  " + strings.Join(details, p.quiet(" · "))
	}
	detailsFit := cellWidth(prefix)+8+cellWidth(stripANSI(detailText)) <= options.Width
	nameBudget := options.Width - cellWidth(prefix)
	if detailsFit {
		nameBudget -= cellWidth(stripANSI(detailText))
	}
	nameBudget = max(1, nameBudget)
	b.WriteString(prefix)
	b.WriteString(p.bold(truncateCells(Name(account), nameBudget)))
	if detailsFit {
		b.WriteString(detailText)
	}
	b.WriteByte('\n')
	if !detailsFit && len(plainDetails) > 0 {
		writeIndented(b, p, strings.Join(plainDetails, " · "), options.Width, false)
	}

	alias := safeLabel(account.Alias)
	email := safeLabel(account.Email)
	if email != "" && (alias == "" || !strings.EqualFold(alias, email)) {
		fmt.Fprintf(b, "  %s\n", p.quiet(truncateCells(email, max(8, options.Width-2))))
	}

	windowCount := 0
	if entry.Quota != nil {
		observedAt := entry.CheckedAt
		if !entry.Quota.ObservedAt.IsZero() {
			observedAt = entry.Quota.ObservedAt
		}
		windowCount += writeWindow(b, p, 1, entry.Quota.Primary, observedAt, now, options)
		windowCount += writeWindow(b, p, 2, entry.Quota.Secondary, observedAt, now, options)
	}

	status := accountStatus(entry, found, cached, now)
	if windowCount == 0 && status == "" {
		status = "quota unavailable"
	}
	if status != "" {
		writeIndented(b, p, status, options.Width, true)
	}
	if warning := safeLabel(entry.Warning); warning != "" {
		writeIndented(b, p, "warning: "+warning, options.Width, false)
	}
}

func writeIndented(b *strings.Builder, p palette, value string, width int, quiet bool) {
	limit := max(8, width-2)
	for len(value) > 0 {
		line := prefixCells(value, limit)
		if len(line) < len(value) {
			cut := strings.LastIndex(line, " ")
			if cut > 0 {
				line = line[:cut]
			}
		}
		styled := line
		if quiet {
			styled = p.quiet(line)
		} else if strings.HasPrefix(line, "warning:") {
			styled = p.ansi("33", "warning:") + strings.TrimPrefix(line, "warning:")
		}
		fmt.Fprintf(b, "  %s\n", styled)
		value = strings.TrimSpace(value[len(line):])
	}
}

func writeWindow(b *strings.Builder, p palette, position int, window *auth.Window, observedAt, now time.Time, options AccountOptions) int {
	if window == nil {
		return 0
	}
	label := windowLabel(window, position)
	reset := formatReset(window, observedAt, now)
	if window.UsedPercent == nil || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) {
		writeWindowLine(b, p, options.Width, fmt.Sprintf("  %-8s %s", label, p.quiet("usage unknown")), reset)
		return 1
	}
	remaining := clamp(100 - *window.UsedPercent)
	percent := p.ansi(quotaColor(remaining), fmt.Sprintf("%9s", remainingText(remaining)))
	line := fmt.Sprintf("  %-8s %s", label, percent)
	if !options.Plain && options.Width >= 48 {
		line = fmt.Sprintf("  %-8s %s  %s", label, quotaBar(p, remaining), percent)
	}
	writeWindowLine(b, p, options.Width, line, reset)
	return 1
}

func writeWindowLine(b *strings.Builder, p palette, width int, line, reset string) {
	if reset == "" || cellWidth(stripANSI(line+reset)) <= width {
		b.WriteString(line)
		b.WriteString(reset)
		b.WriteByte('\n')
		return
	}
	b.WriteString(line)
	b.WriteByte('\n')
	writeIndented(b, p, strings.TrimPrefix(reset, " · "), width, true)
}

func quotaBar(p palette, remaining float64) string {
	filled := int(math.Round(remaining / (100 / barWidth)))
	filled = max(0, min(barWidth, filled))
	return p.ansi(quotaColor(remaining), strings.Repeat("▰", filled)) + p.quiet(strings.Repeat("▱", barWidth-filled))
}

func quotaColor(remaining float64) string {
	code := "36"
	if remaining <= 10 {
		code = "31;1"
	} else if remaining <= 30 {
		code = "33"
	}
	return code
}

func remainingText(remaining float64) string {
	if remaining > 0 && remaining < 1 {
		return "<1% left"
	}
	return fmt.Sprintf("%.0f%% left", remaining)
}

func accountStatus(entry quota.Entry, found, cached bool, now time.Time) string {
	var status []string
	if entry.LoginRequired {
		status = append(status, "login needed")
	} else if !found || entry.Quota == nil {
		status = append(status, "quota unavailable")
	}
	if entry.Quota != nil && entry.Quota.Exhausted != nil && *entry.Quota.Exhausted {
		status = append(status, "limit reached")
	}
	if entry.Stale {
		status = append(status, "stale")
	}
	if cached {
		status = append(status, "cached")
	}
	if !entry.CheckedAt.IsZero() {
		status = append(status, "checked "+formatAge(entry.CheckedAt, now))
	}
	return strings.Join(status, " · ")
}

func formatReset(window *auth.Window, observedAt, now time.Time) string {
	if window.ResetsAt != nil {
		if !window.ResetsAt.After(now) {
			return " · reset due " + window.ResetsAt.In(now.Location()).Format("Jan 2 15:04")
		}
		location := now.Location()
		if location == nil {
			location = time.Local
		}
		return " · resets " + window.ResetsAt.In(location).Format("Mon 15:04")
	}
	if window.ResetAfter != nil {
		remaining := *window.ResetAfter
		if !observedAt.IsZero() && now.After(observedAt) {
			remaining -= now.Sub(observedAt)
		}
		if remaining <= 0 {
			return " · reset due"
		}
		return " · resets in " + formatDuration(remaining)
	}
	return ""
}

func windowLabel(window *auth.Window, position int) string {
	if window.Window == nil || *window.Window <= 0 {
		return fmt.Sprintf("quota %d", position)
	}
	if *window.Window == 7*24*time.Hour {
		return "weekly"
	}
	return formatDuration(*window.Window)
}

func formatDuration(duration time.Duration) string {
	if duration <= 0 {
		return "now"
	}
	if duration < time.Minute {
		return "<1m"
	}
	duration = duration.Round(time.Minute)
	days := duration / (24 * time.Hour)
	duration %= 24 * time.Hour
	hours := duration / time.Hour
	minutes := duration % time.Hour / time.Minute
	parts := make([]string, 0, 2)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 && len(parts) < 2 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 && len(parts) < 2 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	return strings.Join(parts, " ")
}

func formatAge(checkedAt, now time.Time) string {
	if checkedAt.After(now) {
		return "in the future"
	}
	age := now.Sub(checkedAt)
	if age < time.Minute {
		return "just now"
	}
	return formatDuration(age) + " ago"
}

func clamp(value float64) float64 {
	if math.IsNaN(value) {
		return 0
	}
	return max(0, min(100, value))
}

func safeLabel(value string) string {
	var b strings.Builder
	space := false
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			space = b.Len() > 0
			continue
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func truncateLabel(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func truncateCells(value string, limit int) string {
	if cellWidth(value) <= limit {
		return value
	}
	var b strings.Builder
	used := 0
	for _, r := range value {
		width := runeWidth(r)
		if used+width+1 > limit {
			break
		}
		b.WriteRune(r)
		used += width
	}
	return b.String() + "…"
}

func prefixCells(value string, limit int) string {
	used := 0
	end := 0
	for index, r := range value {
		width := runeWidth(r)
		if used+width > limit {
			break
		}
		used += width
		end = index + len(string(r))
	}
	return value[:end]
}

func cellWidth(value string) int {
	width := 0
	for _, r := range value {
		width += runeWidth(r)
	}
	return width
}

func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == '\u200d' {
		return 0
	}
	// The terminal widths relevant to account labels are covered by the common
	// East Asian wide ranges. Ambiguous-width glyphs remain one cell.
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) || (r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1faff) || (r >= 0x20000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

func stripANSI(value string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range value {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
