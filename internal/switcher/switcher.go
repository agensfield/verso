// Package switcher owns the account transition, independently of CLI and RPC transport.
package switcher

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

type Daemon string

const (
	Unknown Daemon = "unknown"
	Stopped Daemon = "stopped"
	Running Daemon = "running"
)

type Inspection struct {
	Daemon       Daemon   `json:"daemon"`
	Active       string   `json:"active"`
	ActiveKnown  bool     `json:"activeKnown"`
	FileBacked   bool     `json:"fileBacked"`
	Busy         []string `json:"busy,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
	Herdr        bool     `json:"herdr"`
	Exhausted    bool     `json:"targetExhausted"`
	QuotaUnknown bool     `json:"targetQuotaUnknown"`
}

type Request struct {
	Target          string `json:"target"`
	AllowExhausted  bool   `json:"allowExhausted"`
	AllowNoSnapshot bool   `json:"allowNoSnapshot"`
}

type Plan struct {
	Request
	Inspection
}

// Checkpoint contains identities and phases only, never credentials.
type Checkpoint struct {
	Version   int    `json:"version"`
	From      string `json:"from"`
	Target    string `json:"target"`
	HadDaemon bool   `json:"hadDaemon"`
	Phase     string `json:"phase"`
}

type Result struct {
	Active            string   `json:"active"`
	ActiveKnown       bool     `json:"activeKnown"`
	Changed           bool     `json:"changed"`
	RollbackAttempted bool     `json:"rollbackAttempted,omitempty"`
	RollbackSucceeded bool     `json:"rollbackSucceeded,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

var (
	ErrUnknown    = errors.New("Codex runtime or active identity is unknown; no switch performed")
	ErrBackend    = errors.New("explicit file-backed Codex credentials are required")
	ErrBusy       = errors.New("active Codex turns block this switch")
	ErrExhausted  = errors.New("target quota is exhausted; explicit override required")
	ErrChanged    = errors.New("switch conditions changed after approval; review a new preview")
	ErrUnfinished = errors.New("an unfinished switch needs human recovery before another switch")
	ErrHuman      = errors.New("human approval is required")
)

// Backend implementations must sanitize errors and keep Inspect strictly read-only.
// Lock serializes Verso operations only. Stop must positively establish daemon exit.
// Activate("") restores a previously logged-out state, removing only owned auth.
// Verify checks daemon identity when running, otherwise the authoritative auth file.
type Backend interface {
	Lock(context.Context) (release func(), err error)
	Unfinished(context.Context) (bool, error)
	Inspect(context.Context, string) (Inspection, error)
	PrepareTarget(context.Context, string) error
	Snapshot(context.Context) error
	WriteCheckpoint(context.Context, Checkpoint) error
	ClearCheckpoint(context.Context) error
	Stop(context.Context) error
	SaveActive(context.Context, string) error
	Activate(context.Context, string) error
	Start(context.Context) error
	Verify(context.Context, string, bool) error
}

type Engine struct{ Backend Backend }

func check(p Plan) error {
	if p.Target == "" {
		return errors.New("target account is required")
	}
	if !p.ActiveKnown || (p.Daemon != Running && p.Daemon != Stopped) {
		return ErrUnknown
	}
	if !p.FileBacked {
		return ErrBackend
	}
	if p.Active == p.Target {
		return nil
	}
	if p.Daemon == Running && len(p.Busy) > 0 {
		return ErrBusy
	}
	if p.Exhausted && !p.AllowExhausted {
		return ErrExhausted
	}
	return nil
}

// Preview does not enroll, refresh, save credentials, snapshot or restart.
func (e Engine) Preview(ctx context.Context, req Request) (Plan, error) {
	state, err := e.Backend.Inspect(ctx, req.Target)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Request: req, Inspection: state}
	return p, check(p)
}

// Execute requires caller-enforced human interaction. There is no yes/force bypass.
// Background and standalone warnings are advisory; normal daemon shutdown is used.
func (e Engine) Execute(ctx context.Context, req Request, approve func(Plan) error) (Result, error) {
	if approve == nil {
		return Result{}, ErrHuman
	}
	release, err := e.Backend.Lock(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	unfinished, err := e.Backend.Unfinished(ctx)
	if err != nil {
		return Result{}, err
	}
	if unfinished {
		return Result{}, ErrUnfinished
	}
	p, err := e.Preview(ctx, req)
	if err != nil {
		// Expired cached quota may be repaired by target preparation, but never
		// bypass runtime/backend/busy refusal to perform a refresh.
		if !errors.Is(err, ErrExhausted) {
			return Result{}, err
		}
	}
	if p.Active == req.Target {
		return Result{Active: p.Active, ActiveKnown: true}, nil
	}
	if err := e.Backend.PrepareTarget(ctx, req.Target); err != nil {
		return Result{Active: p.Active, ActiveKnown: true}, err
	}
	p, err = e.Preview(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if p.Active == req.Target {
		return Result{Active: p.Active, ActiveKnown: true}, nil
	}
	if err := approve(p); err != nil {
		return Result{Active: p.Active, ActiveKnown: true}, err
	}
	fresh, err := e.Preview(ctx, req)
	if err != nil {
		return Result{Active: p.Active, ActiveKnown: true}, err
	}
	if !sameConditions(p.Inspection, fresh.Inspection) {
		return Result{Active: fresh.Active, ActiveKnown: fresh.ActiveKnown}, ErrChanged
	}
	out := Result{Active: p.Active, ActiveKnown: true, Warnings: slices.Clone(p.Warnings)}
	if p.Herdr {
		if err := e.Backend.Snapshot(ctx); err != nil {
			if !req.AllowNoSnapshot {
				return out, fmt.Errorf("Herdr snapshot failed; explicit override required: %w", err)
			}
			out.Warnings = append(out.Warnings, "proceeding without a fresh Herdr snapshot")
		}
	}
	cp := Checkpoint{Version: 1, From: p.Active, Target: req.Target, HadDaemon: p.Daemon == Running, Phase: "prepared"}
	write := func(phase string) error {
		cp.Phase = phase
		return e.Backend.WriteCheckpoint(ctx, cp)
	}
	if err := write("prepared"); err != nil {
		return out, err
	}
	if cp.HadDaemon {
		if err := write("stopping"); err != nil {
			return out, err
		}
		if err := e.Backend.Stop(ctx); err != nil {
			return out, fmt.Errorf("daemon stop could not be confirmed; credentials unchanged, recovery required: %w", err)
		}
	}
	// From this point a stopped daemon may need recovery even before activation.
	if err := e.Backend.SaveActive(ctx, p.Active); err != nil {
		return out, fmt.Errorf("cannot preserve outgoing credentials; recovery required: %w", err)
	}
	if err := write("activating"); err != nil {
		return out, err
	}
	out.Active, out.ActiveKnown = "", false
	if err := e.Backend.Activate(ctx, req.Target); err != nil {
		return e.rollback(ctx, cp, out, fmt.Errorf("target activation failed: %w", err))
	}
	if cp.HadDaemon {
		if err := write("starting"); err != nil {
			return out, fmt.Errorf("target credentials installed but journal update failed; recovery required: %w", err)
		}
		if err := e.Backend.Start(ctx); err != nil {
			return e.rollback(ctx, cp, out, fmt.Errorf("target daemon failed to start: %w", err))
		}
	}
	if err := e.Backend.Verify(ctx, req.Target, cp.HadDaemon); err != nil {
		return e.rollback(ctx, cp, out, fmt.Errorf("target account verification failed: %w", err))
	}
	out.Active, out.Changed = req.Target, true
	out.ActiveKnown = true
	if err := write("committed"); err != nil {
		return out, fmt.Errorf("account switched and verified, but journal needs recovery: %w", err)
	}
	if err := e.Backend.ClearCheckpoint(ctx); err != nil {
		return out, fmt.Errorf("account switched and verified, but journal cleanup failed: %w", err)
	}
	// Client connectivity is intentionally not a success/rollback condition.
	return out, nil
}

func sameConditions(a, b Inspection) bool {
	return a.Daemon == b.Daemon && a.Active == b.Active && a.ActiveKnown == b.ActiveKnown &&
		a.FileBacked == b.FileBacked && a.Herdr == b.Herdr && a.Exhausted == b.Exhausted &&
		a.QuotaUnknown == b.QuotaUnknown && slices.Equal(a.Busy, b.Busy) && slices.Equal(a.Warnings, b.Warnings)
}

func (e Engine) rollback(ctx context.Context, cp Checkpoint, out Result, cause error) (Result, error) {
	out.RollbackAttempted = true
	cp.Phase = "rolling_back"
	if err := e.Backend.WriteCheckpoint(ctx, cp); err != nil {
		return out, errors.Join(cause, fmt.Errorf("cannot journal rollback; human recovery required: %w", err))
	}
	// Never overwrite credentials beneath a possibly running target daemon.
	if cp.HadDaemon {
		if err := e.Backend.Stop(ctx); err != nil {
			return out, errors.Join(cause, fmt.Errorf("rollback cannot confirm daemon stopped: %w", err))
		}
	}
	if err := e.Backend.Activate(ctx, cp.From); err != nil {
		return out, errors.Join(cause, fmt.Errorf("rollback credential restore failed: %w", err))
	}
	if cp.HadDaemon {
		if err := e.Backend.Start(ctx); err != nil {
			return out, errors.Join(cause, fmt.Errorf("rollback daemon start failed: %w", err))
		}
	}
	if err := e.Backend.Verify(ctx, cp.From, cp.HadDaemon); err != nil {
		return out, errors.Join(cause, fmt.Errorf("rollback account verification failed: %w", err))
	}
	out.Active, out.RollbackSucceeded = cp.From, true
	out.ActiveKnown = true
	cp.Phase = "rolled_back"
	if err := e.Backend.WriteCheckpoint(ctx, cp); err != nil {
		return out, errors.Join(cause, err)
	}
	if err := e.Backend.ClearCheckpoint(ctx); err != nil {
		return out, errors.Join(cause, err)
	}
	return out, cause
}
