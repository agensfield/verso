package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
)

const (
	progressDelay = 120 * time.Millisecond
	progressTick  = 80 * time.Millisecond
)

type progressController struct {
	mu      sync.Mutex
	display *progressDisplay
}

type progressDisplay struct {
	mu      sync.Mutex
	out     *os.File
	message string
	width   int
	color   bool
	done    chan struct{}
	stopped chan struct{}
	visible bool
}

func (a *App) progress(format string, args ...any) {
	if !a.progressEnabled() {
		return
	}

	a.progressState.mu.Lock()
	defer a.progressState.mu.Unlock()
	if a.progressState.display == nil {
		a.progressState.display = newProgressDisplay(a.Err.(*os.File), a.progressWidth(), a.progressColor())
	}
	a.progressState.display.update(fmt.Sprintf(format, args...))
}

// clearProgress synchronously removes the active frame. Holding the controller
// lock until the renderer exits prevents a concurrent phase update from racing
// a result, warning, device code, or prompt.
func (a *App) clearProgress() {
	a.progressState.mu.Lock()
	defer a.progressState.mu.Unlock()
	if a.progressState.display == nil {
		return
	}
	a.progressState.display.stop()
	a.progressState.display = nil
}

func (a *App) progressEnabled() bool {
	if a.json || a.plainTerminal() {
		return false
	}
	errOut, ok := a.Err.(*os.File)
	return ok && isatty.IsTerminal(errOut.Fd())
}

func (a *App) progressColor() bool {
	for _, entry := range a.environment() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "NO_COLOR" {
			return false
		}
	}
	return true
}

func (a *App) progressWidth() int {
	errOut, ok := a.Err.(*os.File)
	if ok {
		if width := terminalWidth(errOut.Fd()); width > 0 && width <= 500 {
			return width
		}
	}
	for _, entry := range a.environment() {
		key, value, found := strings.Cut(entry, "=")
		if !found || key != "COLUMNS" {
			continue
		}
		width, err := strconv.Atoi(value)
		if err == nil && width > 0 && width <= 500 {
			return width
		}
	}
	return 80
}

func newProgressDisplay(out *os.File, width int, color bool) *progressDisplay {
	d := &progressDisplay{out: out, width: width, color: color, done: make(chan struct{}), stopped: make(chan struct{})}
	go d.run()
	return d
}

func (d *progressDisplay) update(message string) {
	d.mu.Lock()
	d.message = cleanProgressMessage(message)
	d.mu.Unlock()
}

func (d *progressDisplay) run() {
	defer close(d.stopped)
	timer := time.NewTimer(progressDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-d.done:
		return
	}

	ticker := time.NewTicker(progressTick)
	defer ticker.Stop()
	frame := 0
	for {
		d.render(frame)
		frame++
		select {
		case <-ticker.C:
		case <-d.done:
			d.clear()
			return
		}
	}
}

func (d *progressDisplay) render(frame int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	width := terminalWidth(d.out.Fd())
	if width <= 0 || width > 500 {
		width = d.width
	}
	if width > 1 {
		width--
	}
	frames := "|/-\\"
	mark := string(frames[frame%len(frames)])
	message := ""
	if width > 2 {
		message = fitProgressMessage(d.message, width-2)
	}
	plain := mark
	if message != "" && width > 1 {
		plain += " " + message
	}
	if d.color {
		plain = "\x1b[36m" + mark + "\x1b[0m" + strings.TrimPrefix(plain, mark)
	}
	_, _ = fmt.Fprintf(d.out, "\r\x1b[2K%s", plain)
	d.visible = true
}

func (d *progressDisplay) clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.visible {
		_, _ = fmt.Fprint(d.out, "\r\x1b[2K")
		d.visible = false
	}
}

func (d *progressDisplay) stop() {
	close(d.done)
	<-d.stopped
}

func cleanProgressMessage(message string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, message))
}

func fitProgressMessage(message string, width int) string {
	if width <= 0 {
		return ""
	}
	if progressTextWidth(message) <= width {
		return message
	}
	if width == 1 {
		return "…"
	}
	var fitted strings.Builder
	used := 0
	for _, r := range message {
		runeWidth := progressRuneWidth(r)
		if used+runeWidth > width-1 {
			break
		}
		fitted.WriteRune(r)
		used += runeWidth
	}
	return fitted.String() + "…"
}

func progressTextWidth(value string) int {
	width := 0
	for _, r := range value {
		width += progressRuneWidth(r)
	}
	return width
}

func progressRuneWidth(r rune) int {
	if r == '…' {
		return 1
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if r < utf8.RuneSelf {
		return 1
	}
	// Without a terminal-width framework, counting non-ASCII glyphs as two is
	// deliberately conservative. It prevents wide account names from wrapping.
	return 2
}
