// Package herdr stores one private recovery checkpoint. It never restores panes
// or captures terminal contents, environment variables, or displayed titles.
package herdr

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/agensfield/verso/internal/operation"
)

type Workspace struct {
	ID    string `json:"workspace_id"`
	Label string `json:"label"`
}
type Tab struct {
	ID        string `json:"tab_id"`
	Workspace string `json:"workspace_id"`
	Label     string `json:"label"`
}
type Pane struct {
	ID            string `json:"pane_id"`
	Tab           string `json:"tab_id"`
	Workspace     string `json:"workspace_id"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd,omitempty"`
}
type Session struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Value  string `json:"value"`
}
type Agent struct {
	Name    string   `json:"name,omitempty"`
	Kind    string   `json:"agent"`
	Pane    string   `json:"pane_id"`
	Session *Session `json:"agent_session,omitempty"`
}
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}
type PaneRect struct {
	Pane string `json:"pane_id"`
	Rect Rect   `json:"rect"`
}
type Split struct {
	Direction string  `json:"direction"`
	Ratio     float64 `json:"ratio"`
	Rect      Rect    `json:"rect"`
}
type Layout struct {
	Tab       string     `json:"tab_id"`
	Workspace string     `json:"workspace_id"`
	Area      Rect       `json:"area"`
	Panes     []PaneRect `json:"panes"`
	Splits    []Split    `json:"splits"`
}
type Snapshot struct {
	Schema       string      `json:"schema"`
	CapturedAt   time.Time   `json:"captured_at"`
	HerdrVersion string      `json:"version"`
	Workspaces   []Workspace `json:"workspaces"`
	Tabs         []Tab       `json:"tabs"`
	Panes        []Pane      `json:"panes"`
	Agents       []Agent     `json:"agents"`
	Layouts      []Layout    `json:"layouts"`
}

// Capture accepts `herdr api snapshot` output from the caller's resolved Herdr
// session. Unknown response fields are dropped by the typed whitelist. A failed
// capture never replaces the previous checkpoint. Caller holds the operation lock.
func Capture(root string, raw []byte, now time.Time) error {
	if len(raw) > 8<<20 {
		return errors.New("Herdr snapshot exceeds size limit")
	}
	var envelope struct {
		Result struct {
			Snapshot *Snapshot `json:"snapshot"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Result.Snapshot == nil {
		return errors.New("invalid Herdr snapshot response")
	}
	snapshot := envelope.Result.Snapshot
	if err := validate(snapshot); err != nil {
		return err
	}
	snapshot.Schema = "verso/herdr/v1"
	snapshot.CapturedAt = now.UTC()
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return errors.New("cannot encode Herdr checkpoint")
	}
	return operation.AtomicWrite(root, "herdr-snapshot.json", append(encoded, '\n'))
}

func validate(s *Snapshot) error {
	invalid := errors.New("Herdr snapshot has incomplete or inconsistent locators")
	if s.HerdrVersion == "" || len(s.Workspaces) == 0 {
		return invalid
	}
	workspaces, tabs, panes := map[string]bool{}, map[string]string{}, map[string]string{}
	for _, w := range s.Workspaces {
		if w.ID == "" || workspaces[w.ID] {
			return invalid
		}
		workspaces[w.ID] = true
	}
	for _, t := range s.Tabs {
		if t.ID == "" || tabs[t.ID] != "" || !workspaces[t.Workspace] {
			return invalid
		}
		tabs[t.ID] = t.Workspace
	}
	for _, p := range s.Panes {
		if p.ID == "" || panes[p.ID] != "" || tabs[p.Tab] == "" || tabs[p.Tab] != p.Workspace {
			return invalid
		}
		panes[p.ID] = p.Tab
	}
	for _, a := range s.Agents {
		if panes[a.Pane] == "" {
			return invalid
		}
	}
	for _, l := range s.Layouts {
		if tabs[l.Tab] == "" || tabs[l.Tab] != l.Workspace {
			return invalid
		}
		for _, p := range l.Panes {
			if panes[p.Pane] != l.Tab {
				return invalid
			}
		}
	}
	return nil
}
