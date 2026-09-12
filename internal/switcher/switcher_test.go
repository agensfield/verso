package switcher

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fake struct {
	states     []Inspection
	events     []string
	fail       map[string]error
	unfinished bool
	cp         *Checkpoint
}

func fixture() *fake {
	return &fake{states: []Inspection{{Daemon: Running, Active: "A", ActiveKnown: true, FileBacked: true}}, fail: make(map[string]error)}
}

func (f *fake) event(s string) error { f.events = append(f.events, s); return f.fail[s] }
func (f *fake) Lock(context.Context) (func(), error) {
	return func() { _ = f.event("unlock") }, f.event("lock")
}
func (f *fake) Unfinished(context.Context) (bool, error) { return f.unfinished, f.event("unfinished") }
func (f *fake) Inspect(context.Context, string) (Inspection, error) {
	err := f.event("inspect")
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return s, err
}
func (f *fake) PrepareTarget(context.Context, string) error { return f.event("prepare") }
func (f *fake) Snapshot(context.Context) error              { return f.event("snapshot") }
func (f *fake) WriteCheckpoint(_ context.Context, cp Checkpoint) error {
	if err := f.event("journal:" + cp.Phase); err != nil {
		return err
	}
	f.cp = &cp
	return nil
}
func (f *fake) ClearCheckpoint(context.Context) error {
	if err := f.event("clear"); err != nil {
		return err
	}
	f.cp = nil
	return nil
}
func (f *fake) Stop(context.Context) error                       { return f.event("stop") }
func (f *fake) SaveActive(_ context.Context, s string) error     { return f.event("save:" + s) }
func (f *fake) Activate(_ context.Context, s string) error       { return f.event("activate:" + s) }
func (f *fake) Start(context.Context) error                      { return f.event("start") }
func (f *fake) Verify(_ context.Context, s string, _ bool) error { return f.event("verify:" + s) }
func consent(Plan) error                                         { return nil }
func run(f *fake) (Result, error) {
	return (Engine{Backend: f}).Execute(context.Background(), Request{Target: "B"}, consent)
}
func has(f *fake, event string) bool {
	for _, s := range f.events {
		if s == event {
			return true
		}
	}
	return false
}

func TestCentralSwitchOrdersOutgoingSaveAfterStop(t *testing.T) {
	f := fixture()
	f.states[0].Herdr = true
	out, err := run(f)
	if err != nil || out.Active != "B" || !out.Changed || out.RollbackAttempted {
		t.Fatalf("result=%+v err=%v", out, err)
	}
	want := []string{"lock", "unfinished", "inspect", "prepare", "unfinished", "inspect", "unfinished", "inspect", "snapshot", "journal:prepared", "journal:stopping", "stop", "save:A", "journal:activating", "activate:B", "journal:starting", "start", "verify:B", "journal:committed", "clear", "unlock"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events=%v", f.events)
	}
}

func TestStandaloneDoesNotStartDaemonOrBlockOnTUIActivity(t *testing.T) {
	f := fixture()
	f.states[0].Daemon = Stopped
	f.states[0].Busy = []string{"standalone session"}
	f.states[0].Warnings = []string{"standalone clients may retain account A"}
	out, err := run(f)
	if err != nil || !out.Changed || len(out.Warnings) != 1 {
		t.Fatalf("result=%+v err=%v", out, err)
	}
	if has(f, "stop") || has(f, "start") || has(f, "snapshot") {
		t.Fatalf("unexpected lifecycle: %v", f.events)
	}
}

func TestPreflightRefusalsHaveNoCredentialOrRuntimeSideEffects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fake)
		want   error
	}{
		{"unknown", func(f *fake) { f.states[0].Daemon = Unknown }, ErrUnknown},
		{"identity", func(f *fake) { f.states[0].ActiveKnown = false }, ErrUnknown},
		{"keyring", func(f *fake) { f.states[0].FileBacked = false }, ErrBackend},
		{"busy", func(f *fake) { f.states[0].Busy = []string{"awaiting approval"} }, ErrBusy},
		{"unfinished", func(f *fake) { f.unfinished = true }, ErrUnfinished},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fixture()
			c.mutate(f)
			_, err := run(f)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v", err)
			}
			if has(f, "prepare") || has(f, "stop") || has(f, "activate:B") || f.cp != nil {
				t.Fatalf("side effects: %v", f.events)
			}
		})
	}
}

func TestUnknownQuotaWarnsWhileExhaustedNeedsOverride(t *testing.T) {
	f := fixture()
	f.states[0].QuotaUnknown = true
	if _, err := run(f); err != nil {
		t.Fatal(err)
	}
	f = fixture()
	f.states[0].Exhausted = true
	if _, err := run(f); !errors.Is(err, ErrExhausted) || has(f, "stop") {
		t.Fatalf("err=%v events=%v", err, f.events)
	}
	f = fixture()
	f.states[0].Exhausted = true
	_, err := (Engine{Backend: f}).Execute(context.Background(), Request{Target: "B", AllowExhausted: true}, consent)
	if err != nil {
		t.Fatal(err)
	}
}

func TestActiveAccountNoopEvenWhenBusy(t *testing.T) {
	f := fixture()
	f.states[0].Active = "B"
	f.states[0].Busy = []string{"working"}
	out, err := run(f)
	if err != nil || out.Changed || out.Active != "B" || has(f, "prepare") || has(f, "stop") {
		t.Fatalf("%+v %v %v", out, err, f.events)
	}
}

func TestPrepareAndConsentFailuresLeaveOldRuntimeRunning(t *testing.T) {
	f := fixture()
	f.fail["prepare"] = errors.New("login cancelled")
	if _, err := run(f); err == nil || has(f, "stop") || f.cp != nil {
		t.Fatalf("err=%v events=%v", err, f.events)
	}
	f = fixture()
	_, err := (Engine{Backend: f}).Execute(context.Background(), Request{Target: "B"}, func(Plan) error { return errors.New("declined") })
	if err == nil || has(f, "stop") || f.cp != nil {
		t.Fatalf("err=%v events=%v", err, f.events)
	}
	f = fixture()
	_, err = (Engine{Backend: f}).Execute(context.Background(), Request{Target: "B"}, nil)
	if !errors.Is(err, ErrHuman) || len(f.events) != 0 {
		t.Fatalf("err=%v events=%v", err, f.events)
	}
}

func TestRecheckRefusesNewBusyOrChangedConditions(t *testing.T) {
	for _, change := range []func(*Inspection){
		func(s *Inspection) { s.Active = "C" },
		func(s *Inspection) { s.Daemon = Stopped },
		func(s *Inspection) { s.Warnings = []string{"new background work"} },
		func(s *Inspection) { s.Busy = []string{"new turn"} },
	} {
		f := fixture()
		old := f.states[0]
		fresh := old
		change(&fresh)
		f.states = []Inspection{old, old, fresh}
		_, err := run(f)
		if err == nil || has(f, "stop") || f.cp != nil {
			t.Fatalf("err=%v events=%v", err, f.events)
		}
	}
}

func TestSnapshotFailureRequiresIndependentOverride(t *testing.T) {
	f := fixture()
	f.states[0].Herdr = true
	f.fail["snapshot"] = errors.New("unavailable")
	if _, err := run(f); err == nil || has(f, "stop") {
		t.Fatalf("err=%v events=%v", err, f.events)
	}
	f.events = nil
	out, err := (Engine{Backend: f}).Execute(context.Background(), Request{Target: "B", AllowNoSnapshot: true}, consent)
	if err != nil || !out.Changed || len(out.Warnings) != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestWrongTargetRollsBackOnlyAfterStoppingTarget(t *testing.T) {
	f := fixture()
	f.fail["verify:B"] = errors.New("wrong account")
	out, err := run(f)
	if err == nil || !out.RollbackAttempted || !out.RollbackSucceeded || out.Active != "A" || out.Changed || f.cp != nil {
		t.Fatalf("out=%+v err=%v cp=%+v", out, err, f.cp)
	}
	events := strings.Join(f.events, ",")
	if !strings.Contains(events, "journal:rolling_back,stop,activate:A,start,verify:A") {
		t.Fatal(events)
	}
}

func TestRollbackFailureRetainsJournal(t *testing.T) {
	f := fixture()
	f.fail["verify:B"] = errors.New("wrong account")
	f.fail["activate:A"] = errors.New("disk failed")
	out, err := run(f)
	if err == nil || !out.RollbackAttempted || out.RollbackSucceeded || f.cp == nil || f.cp.Phase != "rolling_back" {
		t.Fatalf("out=%+v err=%v cp=%+v", out, err, f.cp)
	}
}

func TestUnconfirmedStopNeverActivatesOrAutomaticallyRestarts(t *testing.T) {
	f := fixture()
	f.fail["stop"] = errors.New("timeout")
	_, err := run(f)
	if err == nil || has(f, "activate:B") || has(f, "start") || f.cp == nil {
		t.Fatalf("err=%v events=%v", err, f.events)
	}
}

func TestCommittedJournalFailureDoesNotUndoSuccessfulAccount(t *testing.T) {
	for _, event := range []string{"journal:committed", "clear"} {
		f := fixture()
		f.fail[event] = errors.New("disk failed")
		out, err := run(f)
		if err == nil || !out.Changed || out.Active != "B" || out.RollbackAttempted || f.cp == nil {
			t.Fatalf("out=%+v err=%v cp=%+v", out, err, f.cp)
		}
	}
}

func TestPreviewIsReadOnly(t *testing.T) {
	f := fixture()
	p, err := (Engine{Backend: f}).Preview(context.Background(), Request{Target: "B"})
	if err != nil || p.Target != "B" || !reflect.DeepEqual(f.events, []string{"unfinished", "inspect"}) {
		t.Fatalf("p=%+v err=%v events=%v", p, err, f.events)
	}
}

func TestPreviewReportsUnfinishedJournalWithObservedState(t *testing.T) {
	f := fixture()
	f.unfinished = true
	p, err := (Engine{Backend: f}).Preview(context.Background(), Request{Target: "B"})
	if !errors.Is(err, ErrUnfinished) || !p.Unfinished || !p.UnfinishedKnown || p.Target != "B" || p.Active != "A" || !p.ActiveKnown {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestUnfinishedJournalWinsOverInspectionFailure(t *testing.T) {
	f := fixture()
	f.unfinished = true
	f.fail["inspect"] = ErrExhausted
	p, err := (Engine{Backend: f}).Preview(context.Background(), Request{Target: "B"})
	if !errors.Is(err, ErrUnfinished) || !p.Unfinished || !p.UnfinishedKnown || p.Active != "A" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	if _, err := run(f); !errors.Is(err, ErrUnfinished) || has(f, "prepare") {
		t.Fatalf("execute err=%v events=%v", err, f.events)
	}
}

func TestPreviewDistinguishesFailedJournalInspection(t *testing.T) {
	f := fixture()
	f.fail["unfinished"] = errors.New("journal unreadable")
	p, err := (Engine{Backend: f}).Preview(context.Background(), Request{Target: "B"})
	if err == nil || p.Unfinished || p.UnfinishedKnown || p.Target != "B" || p.Active != "A" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestPreviewRetainsPartialInspectionOnError(t *testing.T) {
	f := fixture()
	f.fail["inspect"] = errors.New("inspection incomplete")
	p, err := (Engine{Backend: f}).Preview(context.Background(), Request{Target: "B"})
	if err == nil || p.Target != "B" || p.Active != "A" || !p.ActiveKnown || p.Daemon != Running {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestExecuteReportsActualProgressPhases(t *testing.T) {
	f := fixture()
	f.states[0].Herdr = true
	var phases []string
	_, err := (Engine{Backend: f, OnProgress: func(phase string) {
		phases = append(phases, phase)
	}}).Execute(context.Background(), Request{Target: "B"}, consent)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"preparing", "stopping", "saving", "activating", "starting", "verifying", "committed"}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases=%v want=%v", phases, want)
	}
}

func TestRollbackReportsProgress(t *testing.T) {
	f := fixture()
	f.fail["verify:B"] = errors.New("wrong account")
	var phases []string
	_, _ = (Engine{Backend: f, OnProgress: func(phase string) {
		phases = append(phases, phase)
	}}).Execute(context.Background(), Request{Target: "B"}, consent)
	want := []string{"preparing", "stopping", "saving", "activating", "starting", "verifying", "rolling_back"}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases=%v want=%v", phases, want)
	}
}
