package loop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/internal/orderx"
)

// The recovery carrier uses the original eleven-field packet verbatim.
type ReturnRecoveryPacket = returnPacket

type ReturnRecoveryTarget struct {
	Binding    config.RuntimeSource `json:"binding"`
	Descriptor config.RuntimeSource `json:"descriptor"`
}
type ReturnRecoverySubject struct {
	Request config.RuntimeSource `json:"request"`
	Target  ReturnRecoveryTarget `json:"target"`
	Packet  ReturnRecoveryPacket `json:"packet"`
}
type ReturnRecoveryEvent struct {
	Subject   ReturnRecoverySubject   `json:"subject"`
	Endpoint  string                  `json:"endpoint"`
	Attempt   state.AttemptNode       `json:"original_attempt"`
	Review    state.PendingReviewNode `json:"original_review"`
}
type returnBinding struct {
	Schema  int                  `json:"schema"`
	Request config.RuntimeSource `json:"request"`
	Goal    string               `json:"goal"`
	Main    struct {
		ThreadID string `json:"threadId"`
		HostID   string `json:"hostId"`
	} `json:"main"`
	Connection     string               `json:"connection_instance"`
	Revision       string               `json:"consumer_revision"`
	Producer       config.RuntimeSource `json:"producer"`
	HostInstanceID string               `json:"host_instance_id"`
}

// Select by the retained event, never by the current/last attempt or review.
func MaterializeReturnRecovery(snapshot, history []byte, orderID, eventID string, descriptor, request config.RuntimeSource, host string, executor config.RuntimeSource) (ReturnRecoveryEvent, error) {
	var result ReturnRecoveryEvent
	var s reducer.DurableSnapshot
	if err := decodeReturnRecovery(snapshot, &s); err != nil {
		return result, err
	}
	last, lastErr := strconv.ParseUint(s.State.LastEventID, 10, 64)
	id, idErr := strconv.ParseUint(eventID, 10, 64)
	if s.State.SchemaVersion != statever.Current || !orderx.ValidOrderRevision(s.OrderRevision) || s.GeneratedAt.IsZero() || s.EffectLedger == nil || lastErr != nil || idErr != nil || id == 0 || strconv.FormatUint(id,10) != eventID || last < id {
		return result, fmt.Errorf("original canonical return source incomplete")
	}
	o, ok := s.State.Orders[orderID]
	if !ok || o.OrderID != orderID || len(o.Stages) != 1 || o.Stages[0].StageIndex != 0 || eventID == "" {
		return result, fmt.Errorf("original return order/stage missing")
	}
	stage := o.Stages[0]
	index, count := -1, 0
	for i, a := range stage.Attempts {
		if a.ReturnHandoff != nil && a.ReturnHandoff.EventID == eventID {
			index, count = i, count+1
		}
	}
	if count != 1 {
		return result, fmt.Errorf("original return event missing or ambiguous")
	}
	a := stage.Attempts[index]
	intent := a.ReturnHandoff
	if a.AttemptID == "" || a.SessionID == "" || a.ReturnHandoffUnattempted || intent.Status != "unknown" || a.CompletedAt.IsZero() || a.StartedAt.IsZero() || a.CompletedAt.Before(a.StartedAt) {
		return result, fmt.Errorf("original first return intent incomplete")
	}
	for i, other := range stage.Attempts {
		if i != index && (other.AttemptID == a.AttemptID || other.SessionID == a.SessionID) {
			return result, fmt.Errorf("original attempt/session ambiguous")
		}
	}
	var review state.PendingReviewNode
	current, pending := s.State.PendingReviews[orderID]
	if pending && current.SessionID == a.SessionID {
		review = current
	} else {
		var archive requestChangesRequeued
		if err := decodeReturnRecovery(stage.Extra[requestChangesRequeuedKey], &archive); err != nil {
			return result, fmt.Errorf("original archived review unavailable: %w", err)
		}
		b := archive.Binding
		if b.Attempt != index || b.AttemptID != a.AttemptID || b.SessionID != a.SessionID || b.Reason != a.Error || !cleanupReviewRejected(a) || index+1 >= len(stage.Attempts) || b.WorktreeName != a.WorktreeName || b.WorktreePath != archive.Review.WorktreePath {
			return result, fmt.Errorf("original review revision history differs")
		}
		review = archive.Review
		packet := requestChangesPacket{Order: o, Review: review, Binding: b}
		found, offset := false, 0
		for _, line := range bytes.SplitAfter(history, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) > 0 {
				var e event.LoopEvent
				if json.Unmarshal(line, &e) != nil {
					return result, fmt.Errorf("original review history invalid")
				}
				var p struct { OrderID string `json:"order_id"` }
				if json.Unmarshal(e.Payload, &p) != nil {
					return result, fmt.Errorf("original review history payload invalid")
				}
				if e.Type == event.LoopEventOrderRequeued && p.OrderID == orderID && validateRequestChangesFailure(history[:offset], packet) == nil {
					if found {
						return result, fmt.Errorf("original requeue history ambiguous")
					}
					found = true
				}
			}
			offset += len(line)
		}
		if !found {
			return result, fmt.Errorf("original typed request-changes/requeue history missing")
		}
	}
	if review.OrderID != orderID || review.StageIndex != 0 || review.SessionID != a.SessionID || review.WorktreeName != a.WorktreeName || review.ReturnHandoff == nil || *review.ReturnHandoff != *intent {
		return result, fmt.Errorf("original return review/attempt differs")
	}
	var target returnDescriptor
	if err := readReturnSource(descriptor, &target); err != nil {
		return result, err
	}
	var binding returnBinding
	if err := readReturnSource(target.Binding, &binding); err != nil {
		return result, err
	}
	if target.Schema != 1 || binding.Schema != 1 || target.HostInstanceID != host || host == "" || binding.HostInstanceID != host || target.Producer != executor || binding.Producer != executor || binding.Request != request || binding.Main.HostID != "local" || binding.Main.ThreadID == "" || binding.Connection == "" || binding.Revision == "" || binding.Goal == "" || !filepath.IsAbs(target.Endpoint) || filepath.Clean(target.Endpoint) != target.Endpoint || target.Binding.Path != intent.BindingPath || target.Binding.SHA256 != intent.BindingSHA256 {
		return result, fmt.Errorf("original Frozen return binding differs")
	}
	packet := originalReturnPacket(&target, intent, orderID, 0, a.AttemptID, a.SessionID)
	result = ReturnRecoveryEvent{ReturnRecoverySubject{request, ReturnRecoveryTarget{target.Binding, descriptor}, packet}, target.Endpoint, a, review}
	return result, nil
}

func decodeReturnRecovery(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("return recovery source trailing bytes")
	}
	return nil
}

// Prefix identity survives legitimate appends. Replacement, truncation, mode or
// prefix changes do not. Unsupported file identity remains unknown.
type ReturnRecoveryPrefix struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mode   uint32 `json:"mode"`
}

func ReadReturnRecoveryPrefix(path string, prior *ReturnRecoveryPrefix) (ReturnRecoveryPrefix, []byte, error) {
	var zero ReturnRecoveryPrefix
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real != path || !filepath.IsAbs(path) {
		return zero, nil, fmt.Errorf("return prefix path changed")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return zero, nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return zero, nil, fmt.Errorf("return prefix is not protected regular evidence")
	}
	f, err := os.Open(path)
	if err != nil {
		return zero, nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return zero, nil, fmt.Errorf("return prefix changed during open")
	}
	stat := reflect.Indirect(reflect.ValueOf(opened.Sys()))
	if !stat.IsValid() || stat.Kind() != reflect.Struct {
		return zero, nil, fmt.Errorf("return prefix identity unsupported")
	}
	dev, ino := stat.FieldByName("Dev"), stat.FieldByName("Ino")
	number := func(v reflect.Value) (uint64, bool) {
		if !v.IsValid() {
			return 0, false
		}
		if v.CanUint() {
			return v.Uint(), true
		}
		if v.CanInt() && v.Int() >= 0 {
			return uint64(v.Int()), true
		}
		return 0, false
	}
	d, dok := number(dev); i, iok := number(ino)
	if !dok || !iok || i == 0 {
		return zero, nil, fmt.Errorf("return prefix device/inode unsupported")
	}
	p := ReturnRecoveryPrefix{Path: path, Bytes: opened.Size(), Device: d, Inode: i, Mode: uint32(opened.Mode())}
	if prior != nil { p.Bytes = prior.Bytes }
	if p.Bytes <= 0 || p.Bytes > 64<<20 || opened.Size() < p.Bytes {
		return zero, nil, fmt.Errorf("return prefix missing, truncated or oversized")
	}
	raw := make([]byte, p.Bytes)
	if _, err = io.ReadFull(f, raw); err != nil {
		return zero, nil, err
	}
	p.SHA256 = recoveryDigest(raw)
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) || after.Mode() != opened.Mode() || after.Size() < p.Bytes {
		return zero, nil, fmt.Errorf("return prefix changed during read")
	}
	if prior != nil && p != *prior {
		return zero, nil, fmt.Errorf("original return prefix differs")
	}
	return p, raw, nil
}

// Legacy evidence also requires the caller to exclude other producers of this
// endpoint and other first returns in the retained event/review frame.
func QualifyReturnDialPrefix(raw []byte, original ReturnRecoveryEvent, controller config.RuntimeSource) (legacy bool, err error) {
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return false, fmt.Errorf("original normal log prefix incomplete")
	}
	count := 0
	p := original.Subject.Packet
	exact := map[string]string{
		"endpoint":         original.Endpoint,
		"event_id":         p.EventID,
		"order_id":         p.OrderID,
		"stage_index":      strconv.Itoa(p.StageIndex),
		"attempt_id":       p.AttemptID,
		"session_id":       p.SessionID,
		"host_instance_id": p.HostInstanceID,
		"producer_path":    p.Producer.Path,
		"producer_sha256":  p.Producer.SHA256,
		"binding_path":     p.Binding.Path,
		"binding_sha256":   p.Binding.SHA256,
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if !bytes.Contains(line, []byte("return handoff")) { continue }
		fields, err := returnLogFields(string(line))
		if err != nil {
			return false, err
		}
		if endpoint := fields["endpoint"]; endpoint != "" && endpoint != original.Endpoint {
			if fields["error"] != "dial unix "+endpoint+": connect: connection refused" || fields["transport_phase"] != "dial_before_encode" {
				return false, fmt.Errorf("original return log endpoint ambiguous")
			}
			continue
		}
		count++
		if fields["level"] != "WARN" || fields["msg"] != "return handoff unknown; original owner readback required" || fields["error"] != "dial unix "+original.Endpoint+": connect: connection refused" {
			return false, fmt.Errorf("original transport result is unknown")
		}
		phase, typed := fields["transport_phase"]
		if typed {
			if p.Producer.SHA256 != controller.SHA256 || phase != "dial_before_encode" {
				return false, fmt.Errorf("original typed Dial implementation differs")
			}
			for key, value := range exact {
				if fields[key] != value {
					return false, fmt.Errorf("original typed Dial source/tuple differs: %s", key)
				}
			}
		} else {
			for key := range exact {
				if _, exists := fields[key]; exists {
					return false, fmt.Errorf("original typed Dial phase missing")
				}
			}
			if p.Producer.SHA256 != "d990ef416ffdfae2d1891f7bc96d22c478148788599a9282d87bf8ae8b71da56" {
				return false, fmt.Errorf("original legacy Dial implementation unsupported")
			}
			legacy = true
		}
		at, err := time.Parse(time.RFC3339Nano, fields["time"])
		if err != nil || at.Before(original.Attempt.CompletedAt.Truncate(time.Millisecond)) || at.Sub(original.Attempt.CompletedAt) > time.Second {
			return false, fmt.Errorf("original Dial event time ambiguous")
		}
	}
	if count != 1 {
		return false, fmt.Errorf("original Dial log missing or ambiguous")
	}
	return legacy, nil
}

func returnLogFields(line string) (map[string]string, error) {
	result := map[string]string{}
	for len(line) > 0 {
		line = strings.TrimLeft(line, " ")
		if line == "" {
			break
		}
		key, rest, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("original normal log invalid")
		}
		var value string
		if strings.HasPrefix(rest, "\"") {
			end := 1
			for end < len(rest) {
				if rest[end] == '\\' { end += 2; continue }
				if rest[end] == '"' {
					break
				}
				end++
			}
			if end >= len(rest) {
				return nil, fmt.Errorf("original normal log quote incomplete")
			}
			var err error
			value, err = strconv.Unquote(rest[:end+1])
			if err != nil {
				return nil, err
			}
			line = rest[end+1:]
		} else {
			value, line, _ = strings.Cut(rest, " ")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("original normal log duplicate field")
		}
		result[key] = value
	}
	return result, nil
}

type ReturnCustody struct {
	Registry         config.RuntimeSource `json:"registry"`
	ReceiverInstance string               `json:"receiver_instance"`
	Event            ReturnRecoveryPacket `json:"event"`
}
type ReturnCustodyReadback struct {
	Operation              string                `json:"operation"`
	Schema                 int                   `json:"schema"`
	Owner                  string                `json:"owner"`
	Status                 string                `json:"status"`
	ConsumerQualified      bool                  `json:"consumer_qualified"`
	Subject                ReturnRecoverySubject `json:"subject"`
	ActualController       config.RuntimeSource  `json:"actual_controller"`
	OriginalTargetReceiver json.RawMessage       `json:"original_target_receiver"`
	ActualCurrentConsumer  json.RawMessage       `json:"actual_current_consumer"`
	Custody                *ReturnCustody        `json:"custody"`
	Effects                []string              `json:"effects"`
}

func ReadReturnCustody(endpoint string, subject ReturnRecoverySubject, controller config.RuntimeSource) (ReturnCustodyReadback, error) {
	var result ReturnCustodyReadback
	c, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		return result, err
	}
	defer c.Close()
	if err = c.SetDeadline(time.Now().Add(8*time.Second)); err != nil {
		return result, err
	}
	request := struct {
		Operation string `json:"operation"`
		Schema int `json:"schema"`
		Subject ReturnRecoverySubject `json:"subject"`
		ActualController config.RuntimeSource `json:"actual_controller"`
	}{"worker_return_custody_readback", 1, subject, controller}
	rawRequest, err := json.Marshal(request)
	if err != nil || len(rawRequest)+1 > 65536 {
		return result, fmt.Errorf("return custody request oversized or invalid")
	}
	if err = json.NewEncoder(c).Encode(request); err != nil {
		return result, err
	}
	line, err := bufio.NewReader(io.LimitReader(c, 65537)).ReadBytes('\n')
	if err != nil || len(line) > 65536 {
		return result, fmt.Errorf("return custody readback incomplete or oversized")
	}
	if err = returnReadbackShape(line); err != nil { return result, err }
	if err = decodeReturnRecovery(line, &result); err != nil {
		return result, err
	}
	return result, ValidateReturnCustody(result, subject, controller)
}
func ValidateReturnCustody(r ReturnCustodyReadback, subject ReturnRecoverySubject, controller config.RuntimeSource) error {
	object := func(raw json.RawMessage) bool { var o map[string]json.RawMessage; return json.Unmarshal(raw, &o) == nil && o != nil }
	validRef := func(ref config.RuntimeSource) bool { return len(ref.SHA256) == 64 && strings.Trim(ref.SHA256, "0123456789abcdef") == "" && filepath.IsAbs(ref.Path) }
	if r.Operation != "worker_return_custody_readback" || r.Schema != 1 || r.Owner != "soodles.loop" || r.Subject != subject || r.ActualController != controller || r.Effects == nil || len(r.Effects) != 0 {
		return fmt.Errorf("original custody response binding differs")
	}
	for _, raw := range []json.RawMessage{r.OriginalTargetReceiver,r.ActualCurrentConsumer} {
		if raw != nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && !object(raw) {
			return fmt.Errorf("native consumer readback shape differs")
		}
	}
	if r.Custody != nil && (r.Custody.Event != subject.Packet || !validRef(r.Custody.Registry) || r.Custody.ReceiverInstance == "") { return fmt.Errorf("original custody evidence differs") }
	if r.Status == "unknown" {
		return nil
	}
	if !r.ConsumerQualified || !object(r.OriginalTargetReceiver) || !object(r.ActualCurrentConsumer) {
		return fmt.Errorf("actual native consumer is unqualified")
	}
	switch r.Status {
	case "absent":
		if r.Custody != nil {
			return fmt.Errorf("absent custody response contains receipt")
		}
	case "custodied":
		if r.Custody == nil || r.Custody.Event != subject.Packet || !validRef(r.Custody.Registry) || r.Custody.ReceiverInstance == "" {
			return fmt.Errorf("original custody receipt differs")
		}
	default:
		return fmt.Errorf("original custody status unsupported")
	}
	return nil
}

func ContinueReturnPacket(endpoint string, packet ReturnRecoveryPacket) error {
	return sendReturnHandoff(endpoint, packet)
}

func returnWireObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') { return nil, fmt.Errorf("return wire object missing") }
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil { return nil, err }
		key, ok := token.(string)
		if !ok { return nil, fmt.Errorf("return wire field invalid") }
		if _, exists := fields[key]; exists { return nil, fmt.Errorf("return wire field duplicated") }
		var value json.RawMessage
		if err = d.Decode(&value); err != nil { return nil, err }
		fields[key] = value
	}
	if _, err = d.Token(); err != nil || d.Decode(new(any)) != io.EOF { return nil, fmt.Errorf("return wire object incomplete") }
	if len(fields) != len(keys) { return nil, fmt.Errorf("return wire fields differ") }
	for _, key := range keys { if _, ok := fields[key]; !ok { return nil, fmt.Errorf("return wire field missing: %s", key) } }
	return fields, nil
}
func returnReadbackShape(raw []byte) error {
	fields, err := returnWireObject(raw,"operation","schema","owner","status","consumer_qualified","subject","actual_controller","original_target_receiver","actual_current_consumer","custody","effects")
	if err != nil { return err }
	subject, err := returnWireObject(fields["subject"],"request","target","packet")
	if err != nil { return err }
	target, err := returnWireObject(subject["target"],"binding","descriptor")
	if err != nil { return err }
	packetKeys := []string{"operation","schema","binding","event_id","type","order_id","stage_index","attempt_id","session_id","host_instance_id","producer"}
	packet, err := returnWireObject(subject["packet"],packetKeys...)
	if err != nil { return err }
	refs := []json.RawMessage{fields["actual_controller"],subject["request"],target["binding"],target["descriptor"],packet["binding"],packet["producer"]}
	if !bytes.Equal(bytes.TrimSpace(fields["custody"]),[]byte("null")) {
		custody, err := returnWireObject(fields["custody"],"registry","receiver_instance","event")
		if err != nil { return err }
		event, err := returnWireObject(custody["event"],packetKeys...)
		if err != nil { return err }
		refs = append(refs,custody["registry"],event["binding"],event["producer"])
	}
	for _, ref := range refs { if _, err = returnWireObject(ref,"path","sha256"); err != nil { return err } }
	return nil
}
