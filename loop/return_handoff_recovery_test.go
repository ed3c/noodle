package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

type returnLogCapture chan []byte
func (c returnLogCapture) Write(p []byte) (int, error) { c <- append([]byte(nil), p...); return len(p), nil }

type recoveryEventFixture struct {
	loop *Loop
	cook *cookHandle
	snapshot reducer.DurableSnapshot
	event ReturnRecoveryEvent
	log []byte
}
func newRecoveryEventFixture(t *testing.T) recoveryEventFixture {
	t.Helper()
	shortRoot := os.TempDir()
	if info, statErr := os.Stat("/tmp"); statErr == nil && info.IsDir() { shortRoot = "/tmp" }
	dir, err := os.MkdirTemp(shortRoot, "nrr-")
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	endpoint := filepath.Join(dir, "r.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil { t.Fatal(err) }
	listener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil { t.Fatal(err) }
	l, cook := returnHandoffTestLoop(t, endpoint)
	log := make(returnLogCapture, 4)
	l.logger = slog.New(slog.NewTextHandler(log, nil))
	l.eventCounter.Store(5)
	if err := l.parkPendingReview(cook, "original candidate"); err != nil { t.Fatal(err) }
	var line []byte
	select { case line = <-log: case <-time.After(3*time.Second): t.Fatal("original producer log missing") }
	snapshot, err := reducer.ReadSnapshot(l.canonicalSnapshotPath())
	if err != nil { t.Fatal(err) }
	raw, _ := json.Marshal(snapshot)
	var descriptor returnDescriptor
	if err = readReturnSource(l.config.Runtime.ReturnHandoff, &descriptor); err != nil { t.Fatal(err) }
	var binding returnBinding
	if err = readReturnSource(descriptor.Binding, &binding); err != nil { t.Fatal(err) }
	original, err := MaterializeReturnRecovery(raw, nil, cook.orderID, "6", l.config.Runtime.ReturnHandoff, binding.Request, descriptor.HostInstanceID, descriptor.Producer)
	if err != nil { t.Fatal(err) }
	return recoveryEventFixture{l, cook, snapshot, original, line}
}
func (f recoveryEventFixture) materialize(t *testing.T, snapshot reducer.DurableSnapshot, history []byte, eventID string) (ReturnRecoveryEvent, error) {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil { t.Fatal(err) }
	return MaterializeReturnRecovery(raw, history, f.cook.orderID, eventID, f.loop.config.Runtime.ReturnHandoff, f.event.Subject.Request, f.event.Subject.Packet.HostInstanceID, f.event.Subject.Packet.Producer)
}
func (f recoveryEventFixture) historical(t *testing.T) (reducer.DurableSnapshot, []byte) {
	t.Helper()
	s := f.snapshot
	s.State = s.State.Clone()
	o := s.State.Orders[f.cook.orderID]
	a := &o.Stages[0].Attempts[0]
	a.Status, a.Error = state.AttemptFailed, "changes requested: original revision"
	review := s.State.PendingReviews[o.OrderID]
	binding := requestChangesBinding{Attempt: 0, AttemptID: a.AttemptID, SessionID: a.SessionID, Reason: a.Error, WorktreeName: a.WorktreeName, WorktreePath: review.WorktreePath}
	archive, err := json.Marshal(requestChangesRequeued{Binding: binding, Review: review})
	if err != nil { t.Fatal(err) }
	o.Stages[0].Extra = map[string]json.RawMessage{requestChangesRequeuedKey: archive}
	o.Stages[0].Attempts = append(o.Stages[0].Attempts, state.AttemptNode{AttemptID: dispatchAttemptID(o.OrderID, 0, 1), SessionID: "new-running-session", Status: state.AttemptRunning, ReturnHandoffUnattempted: true})
	o.Stages[0].Status = state.StageRunning
	s.State.Orders[o.OrderID] = o
	delete(s.State.PendingReviews, o.OrderID)
	mistake := newCookMistakeEnvelope(CookMistakeReasonRequestChanges, o.OrderID, 0)
	failure := eventFailureMetadataForLoop(CycleFailureClassOrderHard, OrderFailureClassStageTerminal, &mistake)
	var history bytes.Buffer
	enc := json.NewEncoder(&history)
	for i, entry := range []struct { kind event.LoopEventType; payload any }{
		{event.LoopEventStageFailed, StageFailedPayload{OrderID: o.OrderID, StageIndex: 0, SessionID: &a.SessionID, Reason: a.Error, AgentMistake: &mistake, Failure: &failure}},
		{event.LoopEventOrderFailed, OrderFailedPayload{OrderID: o.OrderID, Reason: a.Error, AgentMistake: &mistake, Failure: &failure}},
		{event.LoopEventOrderRequeued, OrderRequeuedPayload{OrderID: o.OrderID}},
	} {
		raw, _ := json.Marshal(entry.payload)
		if err = enc.Encode(event.LoopEvent{Seq: uint64(i+1), Type: entry.kind, At: time.Now(), Payload: raw}); err != nil { t.Fatal(err) }
	}
	return s, history.Bytes()
}
func TestReturnRecoveryOriginalProducerAndHistoricalAttempt(t *testing.T) {
	f := newRecoveryEventFixture(t)
	if f.event.Subject.Packet.EventID != "6" || f.event.Subject.Packet.AttemptID != dispatchAttemptID(f.cook.orderID, 0, 0) { t.Fatal("original packet identity changed") }
	if err := QualifyReturnDialPrefix(f.log, f.event, f.event.Subject.Packet.Producer); err != nil { t.Fatal(err) }
	s, history := f.historical(t)
	before, _ := json.Marshal(s)
	e, err := f.materialize(t, s, history, "6")
	if err != nil || e.Subject != f.event.Subject || e.Endpoint != f.event.Endpoint || e.Attempt.Status != state.AttemptFailed { t.Fatal("historical event6 refused or replaced", err, e.Subject) }
	after, _ := json.Marshal(s)
	if !bytes.Equal(before, after) || len(s.State.PendingReviews) != 0 || s.State.Orders[f.cook.orderID].Stages[0].Attempts[1].Status != state.AttemptRunning { t.Fatal("historical readback mutated new attempt") }
	for _, mode := range []string{"last-attempt", "event-replacement", "missing-archive", "missing-history", "foreign-history", "duplicate-attempt", "wrong-session", "changed-intent"} {
		t.Run(mode, func(t *testing.T) {
			changed := s; changed.State = s.State.Clone(); o := changed.State.Orders[f.cook.orderID]; h := history; id := "6"
			switch mode {
			case "last-attempt": o.Stages[0].Attempts[0].ReturnHandoff = nil
			case "event-replacement": id = "7"
			case "missing-archive": delete(o.Stages[0].Extra, requestChangesRequeuedKey)
			case "missing-history": h = nil
			case "foreign-history": h = bytes.ReplaceAll(h, []byte(f.cook.orderID), []byte("foreign"))
			case "duplicate-attempt": o.Stages[0].Attempts = append(o.Stages[0].Attempts, o.Stages[0].Attempts[0])
			case "wrong-session": o.Stages[0].Attempts[0].SessionID = "new-running-session"
			case "changed-intent": v := *o.Stages[0].Attempts[0].ReturnHandoff; v.BindingSHA256 = strings.Repeat("0", 64); o.Stages[0].Attempts[0].ReturnHandoff = &v
			}
			changed.State.Orders[o.OrderID] = o
			if _, err := f.materialize(t, changed, h, id); err == nil { t.Fatal("historical ambiguity accepted", mode) }
		})
	}
}
func TestReturnRecoveryDialEvidenceDiscriminators(t *testing.T) {
	f := newRecoveryEventFixture(t)
	for _, mode := range []string{"missing", "foreign-endpoint", "write", "ack", "lost-ack", "duplicate", "truncated", "time", "tuple", "source", "phase"} {
		t.Run(mode, func(t *testing.T) {
			raw := append([]byte(nil), f.log...); e := f.event
			switch mode {
			case "missing": raw = nil
			case "foreign-endpoint": raw = bytes.ReplaceAll(raw, []byte(e.Endpoint), []byte("/foreign.sock"))
			case "write": raw = bytes.ReplaceAll(raw, []byte("dial unix"), []byte("write unix"))
			case "ack": raw = bytes.ReplaceAll(raw, []byte("dial unix"), []byte("read unix"))
			case "lost-ack": raw = []byte("time="+e.Attempt.CompletedAt.Format(time.RFC3339Nano)+" level=WARN msg=\"return handoff unknown; original owner readback required\" error=EOF\n")
			case "duplicate": raw = append(raw, raw...)
			case "truncated": raw = raw[:len(raw)-1]
			case "time": e.Attempt.CompletedAt = e.Attempt.CompletedAt.Add(-time.Hour)
			case "tuple": e.Subject.Packet.SessionID = "foreign-session"
			case "source": e.Subject.Packet.Producer.SHA256 = strings.Repeat("1", 64)
			case "phase": raw = bytes.ReplaceAll(raw, []byte("dial_before_encode"), []byte("ack_unknown"))
			}
			if err := QualifyReturnDialPrefix(raw, e, f.event.Subject.Packet.Producer); err == nil { t.Fatal("uncertain transport qualified", mode) }
		})
	}
	legacy := []byte(fmt.Sprintf("time=%s level=WARN msg=%q error=%q\n", f.event.Attempt.CompletedAt.Format(time.RFC3339Nano), "return handoff unknown; original owner readback required", "dial unix "+f.event.Endpoint+": connect: connection refused"))
	if QualifyReturnDialPrefix(legacy, f.event, f.event.Subject.Packet.Producer) == nil { t.Fatal("unsupported legacy implementation qualified") }
	e := f.event; e.Subject.Packet.Producer.SHA256 = "d990ef416ffdfae2d1891f7bc96d22c478148788599a9282d87bf8ae8b71da56"
	if err := QualifyReturnDialPrefix(legacy, e, f.event.Subject.Packet.Producer); err != nil { t.Fatal("inspected legacy source refused", err) }
}
func TestReturnRecoveryProtectedGrowingPrefix(t *testing.T) {
	f := newRecoveryEventFixture(t)
	for _, mode := range []string{"append", "change", "truncate", "replace", "mode", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir()); if err != nil { t.Fatal(err) }
			path := filepath.Join(dir, "host.log")
			if err = os.WriteFile(path, f.log, 0600); err != nil { t.Fatal(err) }
			prefix, _, err := ReadReturnRecoveryPrefix(path, nil); if err != nil { t.Fatal(err) }
			switch mode {
			case "append": file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0); if err != nil { t.Fatal(err) }; _, err = file.WriteString("legitimate later log\n"); _ = file.Close(); if err != nil { t.Fatal(err) }
			case "change": raw := append([]byte(nil), f.log...); raw[0] ^= 1; err = os.WriteFile(path, raw, 0600)
			case "truncate": err = os.Truncate(path, 1)
			case "replace": err = os.Rename(path, path+".old"); if err == nil { err = os.WriteFile(path, f.log, 0600) }
			case "mode": err = os.Chmod(path, 0644)
			case "symlink": err = os.Rename(path, path+".old"); if err == nil { err = os.Symlink(path+".old", path) }
			}
			if err != nil { t.Fatal(err) }
			_, raw, err := ReadReturnRecoveryPrefix(path, &prefix)
			if mode == "append" { if err != nil || !bytes.Equal(raw, f.log) { t.Fatal("append invalidated original prefix", err) } } else if err == nil { t.Fatal("changed protected prefix accepted", mode) }
		})
	}
}
func TestReturnCustodyContractStatesAndConsumerNeighbors(t *testing.T) {
	f := newRecoveryEventFixture(t)
	subject, controller := f.event.Subject, f.event.Subject.Packet.Producer
	base := ReturnCustodyReadback{Operation: "worker_return_custody_readback", Schema: 1, Owner: "soodles.loop", Status: "absent", ConsumerQualified: true, Subject: subject, ActualController: controller, OriginalTargetReceiver: json.RawMessage(`{"instance":"old"}`), ActualCurrentConsumer: json.RawMessage(`{"instance":"fresh-qualified-same-Main"}`), Effects: []string{}}
	if err := ValidateReturnCustody(base, subject, controller); err != nil { t.Fatal("fresh qualified generation refused", err) }
	for _, mode := range []string{"unknown", "custodied", "stale", "unqualified", "foreign-subject", "foreign-controller", "foreign-receipt", "unsupported", "effects", "missing-effects"} {
		t.Run(mode, func(t *testing.T) {
			r := base; valid := false
			switch mode {
			case "unknown": r.Status, r.ConsumerQualified, r.ActualCurrentConsumer = "unknown", false, nil; valid = true
			case "custodied", "foreign-receipt": r.Status = "custodied"; r.Custody = &ReturnCustody{Registry: config.RuntimeSource{Path: "/owner/registry.json", SHA256: strings.Repeat("a", 64)}, ReceiverInstance: "fresh", Event: subject.Packet}; if mode == "custodied" { valid = true } else { r.Custody.Event.AttemptID = "foreign" }
			case "stale": r.ActualCurrentConsumer = nil
			case "unqualified": r.ConsumerQualified = false
			case "foreign-subject": r.Subject.Packet.SessionID = "foreign"
			case "foreign-controller": r.ActualController.Path = "/copied/same-bytes"
			case "unsupported": r.Status = "refused"
			case "effects": r.Effects = []string{"wake"}
			case "missing-effects": r.Effects = nil
			}
			if err := ValidateReturnCustody(r, subject, controller); (err == nil) != valid { t.Fatal(mode, err) }
		})
	}
}

func TestReturnCustodyExactV2WireShape(t *testing.T) {
	f:=newRecoveryEventFixture(t)
	r:=ReturnCustodyReadback{Operation:"worker_return_custody_readback",Schema:1,Owner:"soodles.loop",Status:"absent",ConsumerQualified:true,Subject:f.event.Subject,ActualController:f.event.Subject.Packet.Producer,OriginalTargetReceiver:json.RawMessage(`{}`),ActualCurrentConsumer:json.RawMessage(`{}`),Effects:[]string{}}
	raw,err:=json.Marshal(r);if err!=nil {t.Fatal(err)}
	if err=returnReadbackShape(raw);err!=nil {t.Fatal("contract response refused",err)}
	for _, mode:=range []string{"missing-zero-stage","duplicate-status","unknown-field","missing-consumer","missing-custody","trailing-object"} {
		t.Run(mode,func(t *testing.T) {
			changed:=append([]byte(nil),raw...)
			switch mode {
			case "missing-zero-stage":changed=bytes.ReplaceAll(changed,[]byte(`"stage_index":0,`),nil)
			case "duplicate-status":changed=bytes.Replace(changed,[]byte(`"status":"absent"`),[]byte(`"status":"unknown","status":"absent"`),1)
			case "unknown-field":changed=bytes.Replace(changed,[]byte(`"stage_index":0`),[]byte(`"stage_index":0,"future":true`),1)
			case "missing-consumer":changed=bytes.Replace(changed,[]byte(`"actual_current_consumer":{},`),nil,1)
			case "missing-custody":changed=bytes.Replace(changed,[]byte(`"custody":null,`),nil,1)
			case "trailing-object":changed=append(changed,[]byte(`{}`)...)
			}
			if bytes.Equal(raw,changed) {t.Fatal("wire negative did not change bytes",mode)}
			if returnReadbackShape(changed)==nil {t.Fatal("invalid response treated as absent",mode)}
		})
	}
}
