package loop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/config"
)

type returnDescriptor struct {
	Schema         int                  `json:"schema"`
	Binding        config.RuntimeSource `json:"binding"`
	Endpoint       string               `json:"endpoint"`
	Producer       config.RuntimeSource `json:"producer"`
	HostInstanceID string               `json:"host_instance_id"`
}
type returnPacket struct {
	Operation      string               `json:"operation"`
	Schema         int                  `json:"schema"`
	Binding        config.RuntimeSource `json:"binding"`
	EventID        string               `json:"event_id"`
	Type           string               `json:"type"`
	OrderID        string               `json:"order_id"`
	StageIndex     int                  `json:"stage_index"`
	AttemptID      string               `json:"attempt_id"`
	SessionID      string               `json:"session_id"`
	HostInstanceID string               `json:"host_instance_id"`
	Producer       config.RuntimeSource `json:"producer"`
}

func readReturnSource(ref config.RuntimeSource, value any) error {
	raw, err := config.ReadExplicitSource(ref.Path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if len(raw) > 65536 || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return fmt.Errorf("return source changed or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("return source trailing bytes")
	}
	return nil
}
func (l *Loop) prepareReturnHandoff(cook *cookHandle) (*returnDescriptor, error) {
	payload := l.reviewPayloadForCook(cook, "")
	intent, unattempted, err := l.canonical.ReturnHandoffForAttempt(cook.orderID, cook.stageIndex, payload["attempt_id"].(string), payload["session_id"].(string))
	if err != nil {
		return nil, err
	}
	if intent != nil {
		return nil, nil
	}
	ref := l.config.Runtime.ReturnHandoff
	if ref.Path == "" && ref.SHA256 == "" {
		return nil, nil
	}
	if len(l.config.Runtime.AdmittedOrders) != 1 || l.config.Runtime.AdmittedOrders[0] != cook.orderID || cook.stageIndex != 0 {
		return nil, fmt.Errorf("return handoff requires original single admitted order and stage")
	}
	if !unattempted {
		l.logger.Warn("return handoff history unavailable; original Owner readback required", "order", cook.orderID)
		return nil, nil
	}
	var target returnDescriptor
	if err := readReturnSource(ref, &target); err != nil {
		return nil, err
	}
	var binding struct {
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
	if err := readReturnSource(target.Binding, &binding); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if target.Schema != 1 || binding.Schema != 1 || !filepath.IsAbs(target.Endpoint) || binding.Main.HostID != "local" ||
		binding.Main.ThreadID == "" || binding.Connection == "" || binding.Revision == "" || binding.Goal == "" ||
		target.HostInstanceID != l.deps.ReturnHostInstance || target.HostInstanceID != binding.HostInstanceID || target.HostInstanceID == "" ||
		target.Producer != binding.Producer || target.Producer.Path != exe || target.Producer.SHA256 != hex.EncodeToString(sum[:]) ||
		len(l.config.Runtime.AdmittedOrders) != 1 || l.config.Runtime.AdmittedOrders[0] != cook.orderID || cook.stageIndex != 0 {
		return nil, fmt.Errorf("return handoff requires original opt-in Main, Host, binary and single admitted order")
	}
	return &target, nil
}
func sendReturnHandoff(endpoint string, packet returnPacket) error {
	connection, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return err
	}
	if err := json.NewEncoder(connection).Encode(packet); err != nil {
		return err
	}
	// This is carrier custody/readback only, never Main consumption or product acceptance.
	var result struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(connection, 65537)).Decode(&result); err != nil {
		return err
	}
	if result.Status != "acknowledged" {
		return fmt.Errorf("return handoff unresolved: %s", result.Status)
	}
	return nil
}
