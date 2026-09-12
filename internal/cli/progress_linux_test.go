//go:build linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agensfield/verso/internal/auth"
	"github.com/agensfield/verso/internal/updater"
	"golang.org/x/sys/unix"
)

type ptyCapture struct {
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	data   bytes.Buffer
	done   chan struct{}
}

func newPTYCapture(t *testing.T, width uint16) *ptyCapture {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open PTY: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		t.Skipf("unlock PTY: %v", err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		t.Skipf("locate PTY: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Skipf("open PTY slave: %v", err)
	}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: width}); err != nil {
		slave.Close()
		master.Close()
		t.Skipf("size PTY: %v", err)
	}
	capture := &ptyCapture{master: master, slave: slave, done: make(chan struct{})}
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, readErr := master.Read(buffer)
			if n > 0 {
				capture.mu.Lock()
				_, _ = capture.data.Write(buffer[:n])
				capture.mu.Unlock()
			}
			if readErr != nil {
				close(capture.done)
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = capture.slave.Close()
		_ = capture.master.Close()
		select {
		case <-capture.done:
		case <-time.After(time.Second):
			t.Error("PTY reader did not stop")
		}
	})
	return capture
}

func (p *ptyCapture) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.data.String()
}

func waitForPTY(t *testing.T, capture *ptyCapture, contains string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := capture.output(); strings.Contains(got, contains) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PTY output never contained %q: %q", contains, capture.output())
	return ""
}

func TestProgressPTYAnimatesWithinWidthAndClears(t *testing.T) {
	pty := newPTYCapture(t, 20)
	a := &App{Err: pty.slave, Env: []string{"TERM=xterm", "NO_COLOR=1"}}
	a.progress("Checking an intentionally long operation")
	waitForPTY(t, pty, "| Checking")
	waitForPTY(t, pty, "/ Checking")
	a.clearProgress()
	time.Sleep(30 * time.Millisecond)

	got := pty.output()
	if !strings.HasSuffix(got, "\r\x1b[2K") {
		t.Fatalf("spinner left a cursor artifact: %q", got)
	}
	if strings.Contains(got, "\x1b[36m") {
		t.Fatalf("NO_COLOR spinner used color: %q", got)
	}
	for _, frame := range strings.Split(got, "\r\x1b[2K")[1:] {
		if frame == "" {
			continue
		}
		if len([]rune(frame)) > 19 {
			t.Fatalf("frame exceeds terminal width: %q", frame)
		}
	}
}

func TestProgressPTYAdaptsToNarrowerTerminal(t *testing.T) {
	pty := newPTYCapture(t, 80)
	a := &App{Err: pty.slave, Env: []string{"TERM=xterm", "NO_COLOR=1"}}
	a.progress("Checking an intentionally long operation that fits the original terminal")
	waitForPTY(t, pty, "/ Checking")
	beforeResize := len(pty.output())
	if err := unix.IoctlSetWinsize(int(pty.slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 20}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resized := pty.output()[beforeResize:]
		if strings.Count(resized, "\r\x1b[2K") >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.clearProgress()

	resized := pty.output()[beforeResize:]
	frames := strings.Split(resized, "\r\x1b[2K")[1:]
	checked := 0
	for _, frame := range frames {
		if frame == "" {
			continue
		}
		checked++
		if len([]rune(frame)) > 19 {
			t.Fatalf("resized frame can wrap at column 20: %q", frame)
		}
	}
	if checked < 2 || !strings.Contains(resized, "…") {
		t.Fatalf("spinner did not render truncated post-resize frames: %q", resized)
	}
}

func TestProgressPTYClearsBeforePrompt(t *testing.T) {
	pty := newPTYCapture(t, 80)
	a := &App{In: strings.NewReader("\n"), Out: pty.slave, Err: pty.slave, Env: []string{"TERM=xterm"}}
	a.progress("Preparing confirmation")
	waitForPTY(t, pty, "Preparing confirmation")
	if err := a.confirm(context.Background(), "Continue?"); err == nil {
		t.Fatal("empty confirmation unexpectedly accepted")
	}
	waitForPTY(t, pty, "Continue? [y/N]")
	got := pty.output()
	prompt := strings.LastIndex(got, "Continue? [y/N]")
	clear := strings.LastIndex(got[:prompt], "\r\x1b[2K")
	if clear < 0 || strings.Contains(got[prompt:], "Preparing confirmation") {
		t.Fatalf("prompt was not isolated from spinner: %q", got)
	}
}

func TestProgressPTYClearsOnContextCancellation(t *testing.T) {
	pty := newPTYCapture(t, 80)
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{Out: pty.slave, Err: pty.slave, Env: []string{"TERM=xterm"}, Version: "test"}
	a.UpdateAction = func(ctx context.Context, _ bool) (updater.Result, error) {
		<-ctx.Done()
		return updater.Result{}, ctx.Err()
	}
	exited := make(chan int, 1)
	go func() { exited <- a.Run(ctx, []string{"update", "--check"}) }()
	waitForPTY(t, pty, "Checking for updates")
	cancel()
	if code := <-exited; code == 0 {
		t.Fatal("cancelled update succeeded")
	}
	got := waitForPTY(t, pty, "verso: context canceled")
	errorAt := strings.Index(got, "verso: context canceled")
	if clearAt := strings.LastIndex(got[:errorAt], "\r\x1b[2K"); clearAt < 0 {
		t.Fatalf("cancel error appeared before spinner clear: %q", got)
	}
}

func TestProgressDisabledForDumbAndNonTTYOutputs(t *testing.T) {
	pty := newPTYCapture(t, 80)
	a := &App{Err: pty.slave, Env: []string{"TERM=dumb"}}
	a.progress("must stay hidden")
	time.Sleep(progressDelay + progressTick)
	if got := pty.output(); got != "" {
		t.Fatalf("TERM=dumb emitted progress: %q", got)
	}
	a = &App{Err: pty.slave, Env: []string{"TERM=xterm"}, json: true}
	a.progress("JSON must stay hidden")
	time.Sleep(progressDelay + progressTick)
	if got := pty.output(); got != "" {
		t.Fatalf("JSON emitted progress: %q", got)
	}

	var pipe bytes.Buffer
	a = &App{Err: &pipe, Env: []string{"TERM=xterm"}}
	a.progress("must stay hidden")
	time.Sleep(progressDelay + progressTick)
	if pipe.Len() != 0 {
		t.Fatalf("pipe emitted progress: %q", pipe.String())
	}
}

func TestProgressPTYFastOperationNeverDraws(t *testing.T) {
	pty := newPTYCapture(t, 80)
	a := &App{Err: pty.slave, Env: []string{"TERM=xterm"}}
	a.progress("Already done")
	a.clearProgress()
	time.Sleep(progressDelay + progressTick)
	if got := pty.output(); got != "" {
		t.Fatalf("fast operation flashed progress: %q", got)
	}
}

func TestEnrollmentPTYClearsForDeviceCodeAndResult(t *testing.T) {
	pty := newPTYCapture(t, 80)
	a := &App{StateDir: filepath.Join(t.TempDir(), "state"), Out: pty.slave, Err: pty.slave, Env: []string{"TERM=xterm"}}
	release := make(chan struct{})
	a.Auth = deviceFunc(func(_ context.Context, prompt func(auth.DevicePrompt) error) ([]byte, error) {
		if err := prompt(auth.DevicePrompt{VerificationURL: "https://example.test/device", UserCode: "SYNTHETIC-CODE"}); err != nil {
			return nil, err
		}
		<-release
		return quotaAuth("synthetic-account", "synthetic-token"), nil
	})
	exited := make(chan int, 1)
	go func() { exited <- a.add(context.Background(), []string{"work"}) }()
	waitForPTY(t, pty, "SYNTHETIC-CODE")
	waitForPTY(t, pty, "Waiting for sign-in")
	close(release)
	if code := <-exited; code != 0 {
		t.Fatalf("enrollment exited %d: %q", code, pty.output())
	}
	got := waitForPTY(t, pty, "Account saved.")
	codeAt := strings.Index(got, "SYNTHETIC-CODE")
	waitAt := strings.Index(got, "Waiting for sign-in")
	resultAt := strings.Index(got, "Account saved.")
	if codeAt < 0 || waitAt < codeAt || resultAt < waitAt {
		t.Fatalf("device flow output ordering is unsafe: %q", got)
	}
	if clearAt := strings.LastIndex(got[:resultAt], "\r\x1b[2K"); clearAt < waitAt {
		t.Fatalf("result appeared before waiting spinner cleared: %q", got)
	}
}

func TestProgressConcurrentUpdatesAndClear(t *testing.T) {
	pty := newPTYCapture(t, 80)
	a := &App{Err: pty.slave, Env: []string{"TERM=xterm"}}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for n := 0; n < 100; n++ {
				a.progress("worker %d phase %d", worker, n)
			}
		}(worker)
	}
	workers.Wait()
	time.Sleep(progressDelay + progressTick)
	a.clearProgress()
	deadline := time.Now().Add(time.Second)
	for !strings.HasSuffix(pty.output(), "\r\x1b[2K") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := pty.output(); !strings.HasSuffix(got, "\r\x1b[2K") {
		t.Fatalf("concurrent spinner did not clear: %q", got)
	}
}
