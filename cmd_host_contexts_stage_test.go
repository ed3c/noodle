package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func contextTestFollowingStage(t *testing.T) (string, contextRegistration, contextObservation) {
	t.Helper()
	dir, r, o := contextTestRegistration(t)
	composition := *o.Composition
	composition.StageID = "review"
	// The projection does not open these paths or derive control authority from
	// them. The original owner, not the transport, validates the accepted chain.
	o.ActiveStage = &contextActiveStage{
		Request:     contextFileRef{Path: filepath.Join(dir, "following-request.json"), SHA256: strings.Repeat("c", 64)},
		Composition: composition,
		Handoffs:    []contextFileRef{{Path: filepath.Join(dir, "handoff.json"), SHA256: strings.Repeat("d", 64)}},
	}
	return dir, r, o
}

func TestContextActiveStageKeepsRegisteredActionOwner(t *testing.T) {
	dir, r, o := contextTestFollowingStage(t)
	calls := 0
	handler := contextTestHandler(dir, r, func(_ context.Context, actual contextRegistration, action, version string) (contextObservation, error) {
		calls++
		if actual.Request != r.Request || action != "stop" || version != o.Version {
			t.Fatal("active stage replaced registered control authority")
		}
		return o, nil
	})
	body := fmt.Sprintf(`{"id":"drawing","registry_version":%q,"expected_version":%q,"action":"stop"}`, contextVersion(r), o.Version)
	response := contextTestPost(handler, "/api/contexts/action", body, "http://127.0.0.1:12345", "application/json")
	if response.Code != 200 || calls != 1 {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	var got contextObservation
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Request != r.Request || *got.Composition != *o.Composition || got.ActiveStage == nil || got.ActiveStage.Request != o.ActiveStage.Request || len(got.ActiveStage.Handoffs) != 1 {
		t.Fatal("active stage projection lost root or handoff identity")
	}
	response = contextTestPost(handler, "/api/contexts/action", strings.TrimSuffix(body, "}")+`,"request":`+fmt.Sprintf(`%q}`, o.ActiveStage.Request.Path), "http://127.0.0.1:12345", "application/json")
	if response.Code != 400 || calls != 1 {
		t.Fatal("browser supplied stage path reached owner")
	}
}

func TestContextActiveStageRejectsBrokenProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*contextObservation)
	}{
		{"missing", func(o *contextObservation) { o.ActiveStage = nil }},
		{"root replaced", func(o *contextObservation) { o.Request = o.ActiveStage.Request }},
		{"entry stage replaced", func(o *contextObservation) { o.Composition.StageID = "review" }},
		{"foreign catalog", func(o *contextObservation) { o.ActiveStage.Composition.Definition.SHA256 = strings.Repeat("e", 64) }},
		{"foreign composition", func(o *contextObservation) { o.ActiveStage.Composition.ID = "other" }},
		{"invalid stage", func(o *contextObservation) { o.ActiveStage.Composition.StageID = "../review" }},
		{"relative request", func(o *contextObservation) { o.ActiveStage.Request.Path = "following.json" }},
		{"request hash", func(o *contextObservation) { o.ActiveStage.Request.SHA256 = "bad" }},
		{"absent handoffs", func(o *contextObservation) { o.ActiveStage.Handoffs = nil }},
		{"missing handoff", func(o *contextObservation) { o.ActiveStage.Handoffs = []contextFileRef{} }},
		{"relative handoff", func(o *contextObservation) { o.ActiveStage.Handoffs[0].Path = "handoff.json" }},
		{"handoff hash", func(o *contextObservation) { o.ActiveStage.Handoffs[0].SHA256 = "bad" }},
		{"duplicate handoff", func(o *contextObservation) {
			o.ActiveStage.Handoffs = append(o.ActiveStage.Handoffs, o.ActiveStage.Handoffs[0])
		}},
		{"entry request retained", func(o *contextObservation) { o.ActiveStage.Request = o.Request }},
		{"entry stage retained", func(o *contextObservation) { o.ActiveStage.Composition.StageID = o.Composition.StageID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, r, o := contextTestFollowingStage(t)
			tc.mutate(&o)
			if err := validateContextObservation(o, r); err == nil {
				t.Fatal("broken active stage accepted")
			}
		})
	}
}

func TestContextInitialStageAndUnicodeAlias(t *testing.T) {
	_, r, o := contextTestRegistration(t)
	if err := validateContextObservation(o, r); err != nil {
		t.Fatal(err)
	}
	_, r, o = contextTestFollowingStage(t)
	o.ActiveStage.Composition.StageID = "審查_②"
	if err := validateContextObservation(o, r); err != nil {
		t.Fatal(err)
	}
}
