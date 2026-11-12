package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

type hostAdmissionFileRef = loopruntime.SessionReadbackFileRef

type hostUnacceptedAdmission struct {
	Request    hostAdmissionFileRef   `json:"request"`
	InstanceID string                 `json:"host_instance_id"`
	Reason     string                 `json:"reason"`
	Transfer   *hostAdmissionTransfer `json:"transfer,omitempty"`
}

type hostAdmissionTransfer struct {
	Profile             *hostAdmissionFileRef `json:"profile,omitempty"`
	ProfileSourceSHA256 string                `json:"profile_source_sha256,omitempty"`
	Noodle              hostAdmissionFileRef  `json:"noodle"`
	TargetRoot          string                `json:"target_root"`
	TargetInstance      string                `json:"target_instance"`
	Config              hostAdmissionFileRef  `json:"config"`
	RequestPath         string                `json:"request_path"`
	Initial             hostAdmissionFileRef  `json:"initial_request"`
	Successor           hostRequestBinding    `json:"successor"`
}

func admissionBinding(req hostRequest) (hostRequestBinding, hostAdmissionFileRef, error) {
	var binding hostRequestBinding
	reference, err := loopruntime.ReadSessionReadbackFile(req.RequestFile, &binding)
	if err != nil {
		return binding, reference, err
	}
	if binding.Schema != 1 || !validPoolID(binding.RequestID) || binding.HostDir == "" ||
		binding.ProjectDir != req.ProjectDir || binding.RequestID != req.RequestID ||
		binding.ConfigPath != req.ConfigPath || binding.ConfigSHA256 != req.ConfigSHA256 ||
		binding.Mode != req.Mode || binding.CleanupCompleted != req.CleanupCompleted ||
		(binding.Operation != "attach" && binding.Operation != "once") {
		return binding, reference, errors.New("original admission request binding differs")
	}
	return binding, reference, nil
}

// Only the original actor's pre-admission refusal can establish this evidence.
func (h *noodleHost) recordAdmissionRefusal(req hostRequest, response hostResponse) hostResponse {
	if !h.blocked || h.rolling == nil || req.RequestFile == "" {
		return response
	}
	binding, reference, err := admissionBinding(req)
	if err == nil && binding.HostDir != h.controlRoot() {
		err = errors.New("original admission Host root differs")
	}
	if err == nil {
		err = updateHostPool(h.controlRoot(), func(pool *hostPool) error {
			if owner, exists := pool.Owners[binding.RequestID]; exists {
				if owner.NotAdmitted == nil || owner.NotAdmitted.Request != reference {
					return errors.New("prior admission owner cannot be replaced by a refusal")
				}
				return nil
			}
			if h.requestHasExecutionCustody(binding.RequestID) {
				return errors.New("original request has native execution custody")
			}
			original := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: os.Getpid(),
				HostInstanceID: h.instanceID, ProjectDir: binding.ProjectDir, ContextState: "not-admitted",
				Mode: binding.Mode, ConfigPath: binding.ConfigPath, ConfigSHA256: binding.ConfigSHA256,
				BinaryPath: req.AdmissionPath, BinarySHA256: binding.BinarySHA256, RequestID: binding.RequestID,
				HostDir: binding.HostDir, CleanupCompleted: binding.CleanupCompleted}
			current := original
			current.BinaryPath, current.BinarySHA256 = h.binaryPath, h.binarySHA
			pool.Owners[binding.RequestID] = hostTaskOwner{Original: original, Current: current,
				State: "not-admitted", LastAdmission: &response,
				NotAdmitted: &hostUnacceptedAdmission{Request: reference, InstanceID: h.instanceID, Reason: response.Error}}
			return nil
		})
	}
	if err != nil {
		response.Error += "; unaccepted-admission evidence unavailable: " + err.Error()
	}
	return response
}

func (h *noodleHost) requestHasExecutionCustody(requestID string) bool {
	for _, receipt := range h.receipts {
		if receipt.RequestID == requestID {
			return true
		}
	}
	for _, context := range h.contexts {
		if context.receipt.RequestID == requestID {
			return true
		}
	}
	return false
}

func unacceptedAdmissionEvidence(req hostRequest) (hostRequestBinding, hostTaskOwner, error) {
	binding, reference, err := admissionBinding(req)
	if err != nil {
		return binding, hostTaskOwner{}, err
	}
	pool, err := readHostPool(binding.HostDir)
	if err != nil {
		return binding, hostTaskOwner{}, err
	}
	owner, exists := pool.Owners[binding.RequestID]
	if !exists || owner.NotAdmitted == nil {
		return binding, owner, errors.New("original owner has no durable unaccepted-admission evidence; absence is not proof")
	}
	if owner.NotAdmitted.Request != reference || !validPoolID(owner.NotAdmitted.InstanceID) || owner.Original.HostInstanceID != owner.NotAdmitted.InstanceID ||
		owner.NotAdmitted.Reason == "" || owner.Original.ContextID != "" || owner.Current.ContextID != "" || len(owner.Previous) != 0 ||
		owner.Original.ContextState != "not-admitted" || owner.Current.ContextState != "not-admitted" ||
		owner.Original.HostDir != binding.HostDir || owner.Current.HostDir != binding.HostDir ||
		owner.Original.BinarySHA256 != binding.BinarySHA256 || !hostReceiptMatches(req, owner.Current) ||
		!hostReceiptMatches(req, owner.Original) || owner.Current.HostInstanceID != owner.NotAdmitted.InstanceID ||
		owner.LastAdmission == nil || owner.LastAdmission.Status != "unknown_effect" ||
		owner.LastAdmission.Error != owner.NotAdmitted.Reason ||
		(owner.State != "not-admitted" && owner.State != "transfer-prepared" && owner.State != "transferred-unaccepted") {
		return binding, owner, errors.New("original unaccepted admission evidence or owner differs")
	}
	return binding, owner, nil
}

func (h *noodleHost) unacceptedAdmission(req hostRequest) (hostRequestBinding, hostTaskOwner, error) {
	binding, owner, err := unacceptedAdmissionEvidence(req)
	if err != nil {
		return binding, owner, err
	}
	if h.rolling == nil || binding.HostDir != h.controlRoot() || owner.NotAdmitted.InstanceID != h.instanceID {
		return binding, owner, errors.New("original unaccepted admission owner unavailable")
	}
	if h.requestHasExecutionCustody(binding.RequestID) {
		return binding, owner, errors.New("original request has native execution custody")
	}
	return binding, owner, nil
}

func admissionInputRequired(err error) map[string]any {
	return map[string]any{"schema": 1, "owner": "Noodle.admission", "status": "input_required",
		"error": err.Error(), "effects": []string{}, "next": map[string]any{
			"owner": "Noodle", "operation": "original_unaccepted_admission_readback"}}
}

func (h *noodleHost) admissionReadback(req hostRequest) any {
	binding, owner, err := h.unacceptedAdmission(req)
	if err != nil {
		return admissionInputRequired(err)
	}
	return admissionEvidenceReadback(binding, owner)
}

func admissionEvidenceReadback(binding hostRequestBinding, owner hostTaskOwner) any {
	result := map[string]any{"schema": 1, "owner": "Noodle.admission", "status": "not_admitted",
		"request": owner.NotAdmitted.Request, "request_id": binding.RequestID,
		"host_root": binding.HostDir, "host_instance_id": owner.NotAdmitted.InstanceID,
		"original_error": owner.NotAdmitted.Reason, "transfer": owner.NotAdmitted.Transfer, "effects": []string{}}
	if transfer := owner.NotAdmitted.Transfer; transfer != nil {
		reference, current, err := readTransferredAdmission(transfer)
		if err != nil {
			return admissionInputRequired(err)
		}
		result["current_request"], result["current_binding"] = reference, current
	}
	return result
}

func readTransferredAdmission(transfer *hostAdmissionTransfer) (hostAdmissionFileRef, hostRequestBinding, error) {
	var binding hostRequestBinding
	reference, err := loopruntime.ReadSessionReadbackFile(transfer.RequestPath, &binding)
	if err != nil {
		return reference, binding, err
	}
	if err := validateTransferredAdmission(transfer.RequestPath, binding); err != nil {
		return reference, binding, err
	}
	for _, expected := range []hostAdmissionFileRef{transfer.Config, transfer.Noodle} {
		actual, err := loopruntime.ReadSessionReadbackFile(expected.Path, nil)
		if err != nil || actual != expected {
			return reference, binding, errors.New("original transfer config or executable source changed")
		}
	}
	if binding.HostDir != transfer.TargetRoot || binding.RequiredHostInstanceID != transfer.TargetInstance ||
		binding.ConfigPath != transfer.Config.Path || binding.ConfigSHA256 != transfer.Config.SHA256 ||
		binding.BinarySHA256 != transfer.Noodle.SHA256 {
		return reference, binding, errors.New("successor differs from the original transfer owner")
	}
	return reference, binding, nil
}

func verifyTransferredProfile(transfer *hostAdmissionTransfer) error {
	raw, err := os.ReadFile(transfer.Config.Path)
	if err != nil || hostDigest(raw) != transfer.Config.SHA256 {
		return errors.New("original transfer config source changed")
	}
	configuration, err := config.Parse(raw)
	if err != nil {
		return err
	}
	if configuration.Runtime.OpenShell.Profile == "" {
		if transfer.Profile != nil || transfer.ProfileSourceSHA256 != "" {
			return errors.New("original transfer has unbound OpenShell profile evidence")
		}
		return nil
	}
	filename, err := filepath.EvalSymlinks(configuration.Runtime.OpenShell.Profile)
	if err != nil || filename != filepath.Join(transfer.TargetRoot, "openshell", "profile.json") ||
		transfer.Profile == nil || transfer.Profile.Path != filename || transfer.ProfileSourceSHA256 == "" {
		return errors.New("original transfer target profile binding differs")
	}
	actual, err := loopruntime.ReadSessionReadbackFile(filename, nil)
	if err != nil || actual != *transfer.Profile {
		return errors.New("original transfer profile JSON source changed")
	}
	profile, err := loopruntime.LoadOpenShellProfile(filename)
	if err != nil {
		return err
	}
	if profile.SourceSHA256 != transfer.ProfileSourceSHA256 {
		return errors.New("original transfer profile dependency source changed")
	}
	return loopruntime.VerifyOpenShellProfile(profile)
}

type hostAdmissionTargetConfig struct {
	Config              hostAdmissionFileRef
	Profile             *hostAdmissionFileRef
	ProfileSourceSHA256 string
}

func targetAdmissionConfig(binding hostRequestBinding, target, filename string) (hostAdmissionTargetConfig, error) {
	reference, err := loopruntime.ReadSessionReadbackFile(filename, nil)
	proof := hostAdmissionTargetConfig{Config: reference}
	if err != nil {
		return proof, err
	}
	beforeRaw, err := readHostRequestConfig(hostRequest{ProjectDir: binding.ProjectDir, ConfigPath: binding.ConfigPath})
	if err != nil || hostDigest(beforeRaw) != binding.ConfigSHA256 {
		return proof, errors.New("original admission config source unavailable or changed")
	}
	afterRaw, err := os.ReadFile(filename)
	if err != nil {
		return proof, err
	}
	before, err := config.Parse(beforeRaw)
	if err != nil {
		return proof, err
	}
	after, err := config.Parse(afterRaw)
	if err != nil {
		return proof, err
	}
	if before.Runtime.ReturnHandoff != (config.RuntimeSource{}) || after.Runtime.ReturnHandoff != (config.RuntimeSource{}) {
		return proof, errors.New("bound return handoff has no original new-Host rebind; retain original task admission")
	}
	if before.Runtime.OpenShell.Profile != "" {
		profile, err := filepath.EvalSymlinks(after.Runtime.OpenShell.Profile)
		if err != nil || profile != filepath.Join(target, "openshell", "profile.json") {
			return proof, errors.New("successor OpenShell profile differs from the target Host profile")
		}
		originalProfile, err := loopruntime.LoadOpenShellProfile(before.Runtime.OpenShell.Profile)
		if err != nil {
			return proof, err
		}
		targetProfile, err := loopruntime.LoadOpenShellProfile(profile)
		if err != nil {
			return proof, err
		}
		if err := validateAdmissionProfilePolicy(originalProfile, targetProfile); err != nil {
			return proof, err
		}
		if originalProfile.GatewayName == targetProfile.GatewayName || originalProfile.Gateway.Binding.StateRoot == targetProfile.Gateway.Binding.StateRoot {
			return proof, errors.New("successor OpenShell gateway or custody root is shared with the original Host")
		}
		profileRef, err := loopruntime.ReadSessionReadbackFile(targetProfile.SourcePath, nil)
		if err != nil {
			return proof, err
		}
		if err := loopruntime.VerifyOpenShellProfile(targetProfile); err != nil {
			return proof, err
		}
		proof.Profile, proof.ProfileSourceSHA256 = &profileRef, targetProfile.SourceSHA256
		before.Runtime.OpenShell.Profile = after.Runtime.OpenShell.Profile
	}
	if !reflect.DeepEqual(before, after) {
		return proof, errors.New("successor config changes original admitted task policy")
	}
	return proof, nil
}

func validateAdmissionProfilePolicy(original, target loopruntime.OpenShellProfile) error {
	if original.CredentialOrigin.BindingSHA256 == "" || original.CredentialOrigin.BindingSHA256 != target.CredentialOrigin.BindingSHA256 {
		return errors.New("successor OpenShell credential owner policy differs")
	}
	if original.CLI != target.CLI || original.PayloadDirectory != target.PayloadDirectory ||
		original.PayloadSHA256 != target.PayloadSHA256 || original.InstructionVerifier != target.InstructionVerifier ||
		original.ProgressSupervisor != target.ProgressSupervisor || len(original.Routes) != len(target.Routes) ||
		original.Gateway.Version != target.Gateway.Version || original.Gateway.ComputeDriver != target.Gateway.ComputeDriver ||
		original.Gateway.ReadinessTimeout != target.Gateway.ReadinessTimeout ||
		original.Gateway.Binding.Workspace != target.Gateway.Binding.Workspace || len(target.Gateway.Retained) != 0 {
		return errors.New("successor OpenShell dispatch or gateway policy differs")
	}
	for name, route := range original.Routes {
		next, exists := target.Routes[name]
		if !exists || route.Binary != next.Binary || !proto.Equal(route.Spec, next.Spec) {
			return errors.New("successor OpenShell route policy differs")
		}
	}
	// Client digests include per-Host mTLS and endpoint metadata; verify each one.
	for _, profile := range []loopruntime.OpenShellProfile{original, target} {
		if err := loopruntime.VerifyOpenShellClient(profile); err != nil {
			return err
		}
	}
	cli, err := hostReadbackBinaryRef(original.CLI)
	if err != nil {
		return err
	}
	nextCLI, err := hostReadbackBinaryRef(target.CLI)
	if err != nil || cli != nextCLI {
		return errors.New("successor OpenShell client executable differs")
	}
	environment := func(values []string) []string {
		var policy []string
		for _, value := range values {
			key, _, _ := strings.Cut(value, "=")
			if key != "XDG_CONFIG_HOME" && key != "XDG_STATE_HOME" && key != "OPENSHELL_DB_URL" {
				policy = append(policy, value)
			}
		}
		return policy
	}
	resident := func(profile loopruntime.OpenShellProfile) admission.ResidentSpec {
		spec := profile.Gateway.Resident
		spec.Name, spec.Directory = "", ""
		spec.Environment = environment(spec.Environment)
		spec.Argv = append([]string(nil), spec.Argv...)
		for i := 1; i+1 < len(spec.Argv); i++ {
			switch spec.Argv[i] {
			case "--config", "--tls-cert", "--tls-key", "--tls-client-ca", "--db-url":
				spec.Argv[i+1] = ""
				i++
			}
		}
		return spec
	}
	if !reflect.DeepEqual(environment(original.Environment), environment(target.Environment)) ||
		!reflect.DeepEqual(resident(original), resident(target)) {
		return errors.New("successor OpenShell environment or resident policy differs")
	}
	before, err := admissionGatewayPolicy(original.Gateway.Binding.ConfigSource)
	if err != nil {
		return err
	}
	after, err := admissionGatewayPolicy(target.Gateway.Binding.ConfigSource)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return errors.New("successor OpenShell gateway configuration policy differs")
	}
	return nil
}

func admissionGatewayPolicy(filename string) (map[string]any, error) {
	var document map[string]any
	if _, err := toml.DecodeFile(filename, &document); err != nil {
		return nil, err
	}
	openshell, ok := document["openshell"].(map[string]any)
	if !ok {
		return nil, errors.New("OpenShell admission gateway policy is absent")
	}
	gateway, ok := openshell["gateway"].(map[string]any)
	if !ok {
		return nil, errors.New("OpenShell admission gateway policy is absent")
	}
	// These are the exact fields rewritten by MaterializeGoalOpenShellProfile.
	for _, key := range []string{"name", "bind_address", "guest_tls_ca"} {
		delete(gateway, key)
	}
	for section, keys := range map[string][]string{
		"credential_storage": {"key_encryption_key_path"},
		"gateway_jwt":        {"signing_key_path", "public_key_path", "kid_path", "gateway_id"},
	} {
		if value, exists := gateway[section]; exists {
			fields, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("OpenShell admission credential policy is invalid")
			}
			for _, key := range keys {
				delete(fields, key)
			}
			if len(fields) == 0 {
				delete(gateway, section)
			}
		}
	}
	drivers, ok := openshell["drivers"].(map[string]any)
	if !ok {
		return nil, errors.New("OpenShell admission compute policy is absent")
	}
	vm, ok := drivers["vm"].(map[string]any)
	if !ok {
		return nil, errors.New("OpenShell admission VM policy is absent")
	}
	delete(vm, "state_dir")
	return document, nil
}

func (h *noodleHost) admissionTransfer(ctx context.Context, req hostRequest) any {
	binding, owner, err := h.unacceptedAdmission(req)
	if err != nil {
		return admissionInputRequired(err)
	}
	target, err := filepath.EvalSymlinks(req.AdmissionTargetRoot)
	if err != nil || !filepath.IsAbs(target) || target == h.controlRoot() {
		return admissionInputRequired(errors.New("target Host is not a distinct existing canonical root"))
	}
	targetConfig, err := targetAdmissionConfig(binding, target, req.AdmissionConfigPath)
	if err != nil {
		return admissionInputRequired(err)
	}
	raw, err := nativeHostRPC(ctx, target, hostRequest{Operation: "resources"})
	if err != nil {
		return admissionInputRequired(err)
	}
	var targetHost struct {
		Protocol   string             `json:"protocol"`
		PID        int                `json:"host_pid"`
		Rolling    int                `json:"rolling_host_version"`
		Root       string             `json:"host_root"`
		Instance   string             `json:"host_instance_id"`
		BinaryPath string             `json:"binary_path"`
		BinarySHA  string             `json:"binary_sha256"`
		Resources  admission.Snapshot `json:"resources"`
		Admission  struct {
			CanAccept bool `json:"can_accept"`
		} `json:"admission"`
	}
	if json.Unmarshal(raw, &targetHost) != nil || targetHost.Protocol != hostProtocol || targetHost.PID <= 1 ||
		targetHost.Rolling != 1 || targetHost.Root != target || !validPoolID(targetHost.Instance) ||
		!targetHost.Admission.CanAccept || targetHost.Resources.AdmissionPolicy != admission.WorkerAdmissionPolicy ||
		targetHost.Resources.Maximum != admission.DefaultWorkerLimit {
		return admissionInputRequired(errors.New("target Host admission identity or policy differs"))
	}
	binaryRef, err := loopruntime.ReadSessionReadbackFile(targetHost.BinaryPath, nil)
	if err != nil || binaryRef.SHA256 != targetHost.BinarySHA {
		return admissionInputRequired(errors.New("target Host executable source differs"))
	}
	transfer := owner.NotAdmitted.Transfer
	if transfer == nil {
		id, err := hostID()
		if err != nil {
			return admissionInputRequired(err)
		}
		directory := filepath.Join(filepath.Dir(req.RequestFile), "host-transfers", binding.RequestID, targetHost.Instance)
		prior := owner.NotAdmitted.Request
		transfer = &hostAdmissionTransfer{Profile: targetConfig.Profile, ProfileSourceSHA256: targetConfig.ProfileSourceSHA256,
			Noodle: binaryRef, TargetRoot: target, TargetInstance: targetHost.Instance, Config: targetConfig.Config,
			RequestPath: filepath.Join(directory, "request.json"), Initial: hostAdmissionFileRef{Path: filepath.Join(directory, "prepared-request.json")},
			Successor: hostRequestBinding{PriorRequest: &prior, Schema: 1, RequestID: id, Operation: binding.Operation,
				ProjectDir: binding.ProjectDir, ConfigPath: targetConfig.Config.Path, ConfigSHA256: targetConfig.Config.SHA256,
				Mode: binding.Mode, BinarySHA256: binaryRef.SHA256, Status: "busy", HostDir: target,
				RequiredHostInstanceID: targetHost.Instance, CleanupCompleted: binding.CleanupCompleted}}
	} else if transfer.TargetRoot != target || transfer.TargetInstance != targetHost.Instance || transfer.Config != targetConfig.Config || transfer.Noodle != binaryRef ||
		!reflect.DeepEqual(transfer.Profile, targetConfig.Profile) || transfer.ProfileSourceSHA256 != targetConfig.ProfileSourceSHA256 {
		return admissionInputRequired(errors.New("original transfer target or config selection changed"))
	}
	err = updateHostPool(h.controlRoot(), func(pool *hostPool) error {
		current := pool.Owners[binding.RequestID]
		if current.NotAdmitted == nil || !reflect.DeepEqual(current.NotAdmitted, owner.NotAdmitted) {
			return errors.New("original admission changed before transfer preparation")
		}
		current.NotAdmitted.Transfer = transfer
		if current.State != "transferred-unaccepted" {
			current.State = "transfer-prepared"
		}
		pool.Owners[binding.RequestID] = current
		return nil
	})
	if err != nil {
		return admissionInputRequired(err)
	}
	initialRaw, err := json.MarshalIndent(transfer.Successor, "", "  ")
	if err == nil {
		err = writeAdmissionOnce(transfer.Initial.Path, initialRaw)
	}
	if err == nil && owner.State != "transferred-unaccepted" {
		err = writeAdmissionOnce(transfer.RequestPath, initialRaw)
	}
	if err == nil {
		transfer.Initial, err = loopruntime.ReadSessionReadbackFile(transfer.Initial.Path, nil)
	}
	if err != nil {
		return admissionInputRequired(err)
	}
	err = updateHostPool(h.controlRoot(), func(pool *hostPool) error {
		current := pool.Owners[binding.RequestID]
		if current.NotAdmitted == nil || current.NotAdmitted.Transfer == nil ||
			current.NotAdmitted.Transfer.RequestPath != transfer.RequestPath {
			return errors.New("original transfer intent changed")
		}
		current.NotAdmitted.Transfer = transfer
		current.State = "transferred-unaccepted"
		pool.Owners[binding.RequestID] = current
		return nil
	})
	if err != nil {
		return admissionInputRequired(err)
	}
	currentRequest, currentBinding, err := readTransferredAdmission(transfer)
	if err != nil {
		return admissionInputRequired(err)
	}
	return map[string]any{"schema": 1, "owner": "Noodle.admission", "status": "prepared", "request": owner.NotAdmitted.Request,
		"current_request": currentRequest, "current_binding": currentBinding,
		"transfer": transfer, "next": map[string]any{"owner": "Soodles original task admission",
			"operation": "adopt_unaccepted_host_transfer", "input": transfer}, "effects": []string{"preserved_original_admission_transfer"}}
}

func writeAdmissionOnce(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		actual, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(raw, actual) {
			return errors.New("original transfer file differs")
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := file.Write(raw)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	return errors.Join(writeErr, file.Close())
}

func validateTransferredAdmission(path string, binding hostRequestBinding) error {
	if binding.PriorRequest == nil {
		return errors.New("successor has no original admission source")
	}
	var original hostRequestBinding
	ref, err := loopruntime.ReadSessionReadbackFile(binding.PriorRequest.Path, &original)
	if err != nil || ref != *binding.PriorRequest {
		return errors.New("original transferred request source changed")
	}
	pool, err := readHostPool(original.HostDir)
	if err != nil {
		return err
	}
	owner := pool.Owners[original.RequestID]
	if owner.State != "transferred-unaccepted" || owner.NotAdmitted == nil || owner.NotAdmitted.Transfer == nil {
		return errors.New("original transfer preparation is not complete")
	}
	transfer := owner.NotAdmitted.Transfer
	var initial hostRequestBinding
	actual, err := loopruntime.ReadSessionReadbackFile(transfer.Initial.Path, &initial)
	if err != nil || actual != transfer.Initial {
		return errors.New("original transfer snapshot changed")
	}
	initial.EnvironmentSHA256, binding.EnvironmentSHA256 = "", ""
	initial.Status, binding.Status = "", ""
	if transfer.RequestPath != path || !reflect.DeepEqual(initial, binding) {
		return errors.New("successor admission differs from original transfer")
	}
	return verifyTransferredProfile(transfer)
}

func newHostAdmissionReadbackCmd(app *App) *cobra.Command { return newHostAdmissionCmd(app, false) }
func newHostAdmissionTransferCmd(app *App) *cobra.Command { return newHostAdmissionCmd(app, true) }

func newHostAdmissionCmd(app *App, transfer bool) *cobra.Command {
	var requestFile, targetRoot, configPath string
	name, operation := "admission-readback", "admission_readback"
	if transfer {
		name, operation = "admission-transfer", "admission_transfer"
	}
	cmd := &cobra.Command{Use: name, Short: "Read or transfer an original proven unaccepted admission", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var binding hostRequestBinding
			if _, err := loopruntime.ReadSessionReadbackFile(requestFile, &binding); err != nil {
				return err
			}
			req := hostRequest{Operation: operation, RequestFile: requestFile, RequestID: binding.RequestID,
				ProjectDir: binding.ProjectDir, ConfigPath: binding.ConfigPath, ConfigSHA256: binding.ConfigSHA256,
				Mode: binding.Mode, BinarySHA256: binding.BinarySHA256, CleanupCompleted: binding.CleanupCompleted,
				AdmissionTargetRoot: targetRoot, AdmissionConfigPath: configPath}
			raw, err := nativeHostRPC(cmd.Context(), binding.HostDir, req)
			if err != nil {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(admissionInputRequired(err))
			}
			var result map[string]any
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			if result["owner"] != "Noodle.admission" {
				missing := admissionInputRequired(fmt.Errorf("original Host does not provide unaccepted-admission readback: %v", result["error"]))
				missing["missing_capability"] = "unaccepted_admission_readback"
				return json.NewEncoder(cmd.OutOrStdout()).Encode(missing)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original native request binding")
	_ = cmd.MarkFlagRequired("request-file")
	if transfer {
		cmd.Flags().StringVar(&targetRoot, "target-host-root", "", "Original selected target Host root")
		cmd.Flags().StringVar(&configPath, "config-file", "", "Original task owner's frozen successor config")
		_ = cmd.MarkFlagRequired("target-host-root")
		_ = cmd.MarkFlagRequired("config-file")
	}
	return cmd
}

func newHostAdmissionTransferReadbackCmd() *cobra.Command {
	var requestFile string
	cmd := &cobra.Command{Use: "admission-transfer-readback", Short: "Read an original sealed admission transfer without contacting either Host", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var original hostRequestBinding
			if _, err := loopruntime.ReadSessionReadbackFile(requestFile, &original); err != nil {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(admissionInputRequired(err))
			}
			req := hostRequest{RequestFile: requestFile, RequestID: original.RequestID, ProjectDir: original.ProjectDir,
				ConfigPath: original.ConfigPath, ConfigSHA256: original.ConfigSHA256, Mode: original.Mode,
				CleanupCompleted: original.CleanupCompleted}
			binding, owner, err := unacceptedAdmissionEvidence(req)
			if err != nil {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(admissionInputRequired(err))
			}
			if owner.State != "transferred-unaccepted" || owner.NotAdmitted.Transfer == nil {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(admissionInputRequired(errors.New("original admission has no sealed transfer; live original-owner readback required")))
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(admissionEvidenceReadback(binding, owner))
		}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original native request binding")
	_ = cmd.MarkFlagRequired("request-file")
	return cmd
}
