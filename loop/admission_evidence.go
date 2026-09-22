package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/monitor"
)

// This is the orders.json producer's inclusion rule, not the ownership set:
// projection.legacyOrdersFileProjection omits completed and cancelled orders.
func initialProjectionIDs(s state.State) map[string]struct{} {
	ids := nonScheduleOrderIDs(s)
	for id, node := range s.Orders {
		if node.Status == state.OrderCompleted || node.Status == state.OrderCancelled {
			delete(ids, id)
		}
	}
	return ids
}

func readAdmissionFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("evidence is not a regular file: %s", path)
	}
	return os.ReadFile(path)
}

func readAdmissionEvidence(dir string) (reducer.DurableSnapshot, OrdersFile, error) {
	var zero reducer.DurableSnapshot
	path := filepath.Join(dir, "state.snapshot.json")
	data, err := readAdmissionFile(path)
	if err != nil {
		return zero, OrdersFile{}, err
	}
	snapshot, err := reducer.ReadSnapshot(path)
	if err != nil {
		return zero, OrdersFile{}, err
	}
	// ReadSnapshot permits old/incomplete snapshots for bootstrap. This stopped
	// operation cannot bootstrap or interpret absent evidence as an empty owner.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snapshot); err != nil {
		return zero, OrdersFile{}, err
	}
	if !orderx.ValidOrderRevision(snapshot.OrderRevision) || snapshot.State.SchemaVersion != statever.Current || snapshot.State.Orders == nil || snapshot.EffectLedger == nil || snapshot.GeneratedAt.IsZero() {
		return zero, OrdersFile{}, fmt.Errorf("canonical snapshot is incomplete or unsupported")
	}
	if err := validateAdmissionSnapshot(snapshot); err != nil {
		return zero, OrdersFile{}, err
	}
	data, err = readAdmissionFile(filepath.Join(dir, "orders.json"))
	if err != nil {
		return zero, OrdersFile{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return zero, OrdersFile{}, err
	}
	if raw, ok := fields["orders"]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return zero, OrdersFile{}, fmt.Errorf("orders projection is incomplete")
	}
	orders, err := orderx.ParseOrdersStrict(data)
	if err != nil {
		return zero, OrdersFile{}, err
	}
	if err := validateAdmissionProjection(snapshot.State, orders); err != nil {
		return zero, OrdersFile{}, err
	}
	if err := observeAdmissionSessions(dir, snapshot.State); err != nil {
		return zero, OrdersFile{}, err
	}
	return snapshot, orders, nil
}

func validateAdmissionSnapshot(snapshot reducer.DurableSnapshot) error {
	s := snapshot.State
	if s.LastEventID == "" {
		return fmt.Errorf("canonical last_event_id absent")
	}
	if _, err := parseLastEventID(s.LastEventID); err != nil {
		return err
	}
	switch s.Mode {
	case state.RunModeAuto, state.RunModeSupervised, state.RunModeManual:
	default:
		return fmt.Errorf("unknown canonical mode %q", s.Mode)
	}
	if err := s.Validate(); err != nil {
		return err
	}
	for id, order := range s.Orders {
		if strings.TrimSpace(id) == "" || id != order.OrderID || len(order.Stages) == 0 {
			return fmt.Errorf("invalid canonical order %q", id)
		}
		switch order.Status {
		case state.OrderActive, state.OrderPending, state.OrderCompleted, state.OrderCancelled, state.OrderFailed:
		default:
			return fmt.Errorf("unknown canonical order status %q", order.Status)
		}
		for _, stage := range order.Stages {
			switch stage.Status {
			case state.StagePending, state.StageCompleted, state.StageFailed, state.StageSkipped, state.StageCancelled:
			case state.StageReview: // stopped, but its attempt still needs observation
			default:
				return fmt.Errorf("active or ambiguous canonical stage %s/%d: %s", id, stage.StageIndex, stage.Status)
			}
			for _, attempt := range stage.Attempts {
				switch attempt.Status {
				case state.AttemptCompleted, state.AttemptFailed, state.AttemptCancelled:
				default:
					return fmt.Errorf("active or ambiguous canonical attempt %q", attempt.AttemptID)
				}
				if attempt.SessionID == "" {
					return fmt.Errorf("canonical attempt %q has no session evidence", attempt.AttemptID)
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, record := range snapshot.EffectLedger {
		if record.EffectID == "" || record.EffectID != record.Effect.EffectID || seen[record.EffectID] {
			return fmt.Errorf("invalid or duplicate effect identity %q", record.EffectID)
		}
		seen[record.EffectID] = true
		switch record.Status {
		case reducer.EffectLedgerDone, reducer.EffectLedgerFailed:
			if record.Result == nil || record.Result.EffectID != record.EffectID {
				return fmt.Errorf("effect result absent or mismatched: %s", record.EffectID)
			}
		case reducer.EffectLedgerPending:
			// The current loop records declarative dispatch/projection effects
			// without executing them through the ledger. Subject ownership and
			// actual process absence are checked separately; do not rewrite it.
			if record.Result != nil || record.Effect.Type == reducer.EffectMerge {
				return fmt.Errorf("ambiguous pending effect %q", record.EffectID)
			}
		default:
			return fmt.Errorf("active or ambiguous effect %q", record.EffectID)
		}
		if record.Result != nil {
			switch record.Result.Status {
			case reducer.EffectResultCompleted, reducer.EffectResultFailed, reducer.EffectResultCancelled:
			default:
				return fmt.Errorf("ambiguous effect result %q", record.EffectID)
			}
		}
		switch record.Effect.Type {
		case reducer.EffectInitialAdmission:
			var payload struct {
				OrderID  string `json:"order_id"`
				Revision string `json:"initial_revision"`
			}
			if err := json.Unmarshal(record.Effect.Payload, &payload); err != nil {
				return err
			}
			if payload.OrderID == "" || !orderx.ValidOrderRevision(payload.Revision) || initialAdmissionEffectID(payload.OrderID) != record.EffectID {
				return fmt.Errorf("corrupt initial admission effect %q", record.EffectID)
			}
		case reducer.EffectDispatch, reducer.EffectMerge, reducer.EffectWriteProjection, reducer.EffectAck, reducer.EffectCleanup:
			var payload struct {
				OrderID string `json:"order_id"`
			}
			if json.Unmarshal(record.Effect.Payload, &payload) != nil || strings.TrimSpace(payload.OrderID) == "" {
				return fmt.Errorf("corrupt effect payload %q", record.EffectID)
			}
		default:
			return fmt.Errorf("unknown effect type %q", record.Effect.Type)
		}
	}
	return nil
}

func validateAdmissionProjection(s state.State, orders OrdersFile) error {
	return validateStoppedProjection(s, orders, false)
}

func validateStoppedProjection(s state.State, orders OrdersFile, allowReview bool) error {
	ids := map[string]struct{}{}
	for _, order := range orders.Orders {
		if _, ok := ids[order.ID]; ok {
			return fmt.Errorf("duplicate orders projection identity %q", order.ID)
		}
		ids[order.ID] = struct{}{}
		node, ok := s.Orders[order.ID]
		if !ok {
			return fmt.Errorf("orders projection contains noncanonical order %q", order.ID)
		}
		if err := orderx.ValidateOrderStatus(order.Status); err != nil {
			return err
		}
		if len(order.Stages) != len(node.Stages) {
			return fmt.Errorf("orders projection stage count differs for %q", order.ID)
		}
		expectedStatus := OrderStatusActive
		if node.Status == state.OrderFailed {
			expectedStatus = OrderStatusFailed
		}
		if order.Status != expectedStatus {
			return fmt.Errorf("orders projection status differs for %q", order.ID)
		}
		for i, stage := range order.Stages {
			if err := orderx.ValidateStageStatus(stage.Status); err != nil {
				return err
			}
			switch stage.Status {
			case StageStatusActive, StageStatusMerging:
				if !allowReview || node.Stages[i].Status != state.StageReview || stage.Status != StageStatusActive {
					return fmt.Errorf("orders projection has active stage %s/%d", order.ID, i)
				}
			}
			if allowReview && stage.Status != canonicalStageStatusToLegacy(node.Stages[i].Status) {
				return fmt.Errorf("orders projection stage status differs for %s/%d", order.ID, i)
			}
			if stage.TaskKey != node.Stages[i].TaskKey || stage.Prompt != node.Stages[i].Prompt {
				return fmt.Errorf("orders projection stage identity differs for %s/%d", order.ID, i)
			}
		}
	}
	delete(ids, scheduleOrderID)
	if !maps.Equal(ids, initialProjectionIDs(s)) {
		return fmt.Errorf("orders projection differs from canonical projection")
	}
	return nil
}

func validateRetirementSubject(snapshot reducer.DurableSnapshot, orders OrdersFile, data []byte) error {
	compact, err := orderx.ParseCompactOrders(data)
	if err != nil {
		return err
	}
	if compact.InitialRevision == nil || len(compact.Orders) == 0 {
		return fmt.Errorf("not a nonempty INITIAL proposal")
	}
	if _, err := orderx.ExpandCompactOrders(compact); err != nil {
		return err
	}
	owned := nonScheduleOrderIDs(snapshot.State)
	for _, o := range orders.Orders {
		owned[o.ID] = struct{}{}
	}
	admitted := map[string]bool{}
	effectSubjects := map[string]bool{}
	for _, record := range snapshot.EffectLedger {
		admitted[record.EffectID] = true
		var payload struct {
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(record.Effect.Payload, &payload); err != nil {
			return err
		}
		effectSubjects[payload.OrderID] = true
	}
	seen := map[string]bool{}
	for _, o := range compact.Orders {
		_, exists := owned[o.ID]
		if exists || admitted[initialAdmissionEffectID(o.ID)] || effectSubjects[o.ID] || seen[o.ID] || strings.TrimSpace(o.ID) != o.ID || o.ID == "" || o.ID == scheduleOrderID || len(o.Stages) == 0 {
			return fmt.Errorf("initial subject %q is owned, admitted, duplicate, reserved or ambiguous", o.ID)
		}
		seen[o.ID] = true
	}
	return nil
}

func observeAdmissionSessions(dir string, s state.State) error {
	sessionDir := filepath.Join(dir, "sessions")
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return fmt.Errorf("read session evidence: %w", err)
	}
	observed := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("ambiguous session entry %q", entry.Name())
		}
		path := filepath.Join(sessionDir, entry.Name())
		data, err := readAdmissionFile(filepath.Join(path, "meta.json"))
		if err != nil {
			return fmt.Errorf("session %s: %w", entry.Name(), err)
		}
		var meta monitor.SessionMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			return err
		}
		// meta.json is a derived historical observation. A stale running value
		// cannot override fresh absence of both the process and its group, nor
		// can a terminal value override a still-live process below.
		if meta.SessionID != entry.Name() || (meta.Status != monitor.SessionStatusExited && meta.Status != monitor.SessionStatusFailed && meta.Status != monitor.SessionStatusRunning) {
			return fmt.Errorf("active or ambiguous session %q", entry.Name())
		}
		if meta.Runtime != "" && meta.Runtime != "process" {
			return fmt.Errorf("session %q process observation unavailable for runtime %q", entry.Name(), meta.Runtime)
		}
		data, err = readAdmissionFile(filepath.Join(path, "process.json"))
		if err != nil {
			return err
		}
		var process struct {
			PID       int    `json:"pid"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(data, &process); err != nil {
			return err
		}
		if process.SessionID != entry.Name() {
			return fmt.Errorf("mismatched process session %q", entry.Name())
		}
		pid, err := procx.ReadPIDFile(filepath.Join(path, "process.json"))
		if err != nil {
			return err
		}
		absent, err := procx.ProcessGroupAbsent(pid)
		if err != nil {
			return err
		}
		if !absent {
			return fmt.Errorf("session %q process or process group is present", entry.Name())
		}
		observed[entry.Name()] = true
	}
	for _, order := range s.Orders {
		for _, stage := range order.Stages {
			for _, attempt := range stage.Attempts {
				if !observed[attempt.SessionID] {
					return fmt.Errorf("canonical session %q evidence absent", attempt.SessionID)
				}
			}
		}
	}
	return nil
}
