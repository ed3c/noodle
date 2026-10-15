// Package loopview reads display projections. It does not execute owner next actions.
package loopview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const MaxBytes = 16 << 20

type Resource struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
type Stage struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	Status         string   `json:"status"`
	Prompt         string   `json:"prompt"`
	InputRefs      []string `json:"input_refs"`
	OutputRefs     []string `json:"output_refs"`
	ConsumerStatus string   `json:"consumer_status"`
}
type Card struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	SourceRefs []string `json:"source_refs"`
}
type Note struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	SourceRefs []string `json:"source_refs"`
}
type Breakpoint struct {
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	StageID string `json:"stage_id"`
	Message string `json:"message"`
}
type Run struct {
	ID          string       `json:"id"`
	Repository  string       `json:"repository"`
	Mode        string       `json:"mode"`
	Status      string       `json:"status"`
	Stages      []Stage      `json:"stages"`
	Cards       []Card       `json:"cards"`
	Notes       []Note       `json:"notes"`
	Breakpoints []Breakpoint `json:"breakpoints"`
	Resources   []Resource   `json:"resources,omitempty"`
	Next        *string      `json:"next"`
}
type View struct {
	SchemaVersion int        `json:"schema_version"`
	Runs          []Run      `json:"runs"`
	Resources     []Resource `json:"resources,omitempty"`
}

func Empty() View { return View{SchemaVersion: 1, Runs: []Run{}} }

func Decode(data []byte) (View, error) {
	if len(data) > MaxBytes {
		return View{}, fmt.Errorf("loop view exceeds %d bytes", MaxBytes)
	}
	var view View
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&view); err != nil {
		return View{}, fmt.Errorf("decode loop view: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return View{}, fmt.Errorf("loop view contains trailing data")
	}
	if view.SchemaVersion != 1 {
		return View{}, fmt.Errorf("unsupported loop view schema_version %d", view.SchemaVersion)
	}
	if view.Runs == nil {
		return View{}, fmt.Errorf("loop view runs array missing")
	}
	seen := map[string]bool{}
	for i := range view.Runs {
		run := &view.Runs[i]
		if strings.TrimSpace(run.ID) == "" || strings.TrimSpace(run.Repository) == "" || strings.TrimSpace(run.Mode) == "" || strings.TrimSpace(run.Status) == "" {
			return View{}, fmt.Errorf("run %d identity, mode or status missing", i)
		}
		if seen[run.ID] {
			return View{}, fmt.Errorf("duplicate run id %q", run.ID)
		}
		seen[run.ID] = true
		stages := map[string]bool{}
		for j := range run.Stages {
			stage := &run.Stages[j]
			if strings.TrimSpace(stage.ID) == "" || strings.TrimSpace(stage.Kind) == "" || strings.TrimSpace(stage.Status) == "" || strings.TrimSpace(stage.ConsumerStatus) == "" {
				return View{}, fmt.Errorf("run %q stage %d identity or observation missing", run.ID, j)
			}
			if stages[stage.ID] {
				return View{}, fmt.Errorf("run %q duplicate stage id %q", run.ID, stage.ID)
			}
			stages[stage.ID] = true
			if stage.InputRefs == nil {
				stage.InputRefs = []string{}
			}
			if stage.OutputRefs == nil {
				stage.OutputRefs = []string{}
			}
		}
		cards, notes := map[string]bool{}, map[string]bool{}
		for j := range run.Cards {
			card := &run.Cards[j]
			if strings.TrimSpace(card.ID) == "" || cards[card.ID] {
				return View{}, fmt.Errorf("run %q card id missing or duplicated", run.ID)
			}
			cards[card.ID] = true
			if card.SourceRefs == nil {
				card.SourceRefs = []string{}
			}
		}
		for j := range run.Notes {
			note := &run.Notes[j]
			if strings.TrimSpace(note.ID) == "" || notes[note.ID] {
				return View{}, fmt.Errorf("run %q note id missing or duplicated", run.ID)
			}
			notes[note.ID] = true
			if note.SourceRefs == nil {
				note.SourceRefs = []string{}
			}
		}
		for _, point := range run.Breakpoints {
			if point.StageID != "" && !stages[point.StageID] {
				return View{}, fmt.Errorf("run %q breakpoint stage %q missing", run.ID, point.StageID)
			}
		}
		if run.Stages == nil {
			run.Stages = []Stage{}
		}
		if run.Cards == nil {
			run.Cards = []Card{}
		}
		if run.Notes == nil {
			run.Notes = []Note{}
		}
		if run.Breakpoints == nil {
			run.Breakpoints = []Breakpoint{}
		}
	}
	return view, nil
}
