package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

// This is an explicit failed-compute boundary. It never restores a workspace,
// clears a Host fault, settles the original Exec, or admits another attempt.
type storageFailureRecovery struct {
	Schema                     int
	Stop                       loopruntime.SessionReadbackFileRef
	HostResources, HostHistory loopruntime.SessionReadbackFileRef
	HostStatus                 loopruntime.SessionReadbackFileRef
	Launcher, LauncherCall     loopruntime.SessionReadbackFileRef
	LauncherSession            int
	Originals                  map[string]loopruntime.SessionReadbackFileRef
}

func newStorageFailureCmd() *cobra.Command {
	root := &cobra.Command{Use: "storage-failure", Short: "Stop one failed native attempt while preserving its unknown outcome and disk"}
	var request, session, output, implementation, launcher, launcherCall string
	var launcherSession int
	var abandon bool
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !abandon {
			return fmt.Errorf("workspace return abandonment was not selected")
		}
		ref, err := prepareStorageFailure(request, session, output, implementation, launcher, launcherCall, launcherSession)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"status": "prepared", "recovery": ref, "effects": []string{}})
	}}
	for name, ptr := range map[string]*string{"request-file": &request, "session-id": &session, "output": &output, "native-implementation": &implementation, "launcher-intent": &launcher, "launcher-call": &launcherCall} {
		prepare.Flags().StringVar(ptr, name, "", "Exact original input or new external evidence directory")
		_ = prepare.MarkFlagRequired(name)
	}
	prepare.Flags().IntVar(&launcherSession, "launcher-session", 0, "Original launcher execution session")
	_ = prepare.MarkFlagRequired("launcher-session")
	prepare.Flags().BoolVar(&abandon, "abandon-workspace-return", false, "Keep the failed attempt and disk; do not restore or publish its workspace")
	root.AddCommand(prepare)
	var plan, expected string
	stop := &cobra.Command{Use: "stop", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var recovery storageFailureRecovery
		ref, err := loopruntime.ReadSessionReadbackFile(plan, &recovery)
		if err != nil {
			return err
		}
		if expected != ref.SHA256 || recovery.Schema != 1 {
			return fmt.Errorf("storage failure recovery changed or has unsupported schema")
		}
		var p loopruntime.StorageFailureStopPlan
		if err := storageFailureRef(recovery.Stop, &p); err != nil {
			return err
		}
		var binding hostRequestBinding
		if err := storageFailureRef(p.Request, &binding); err != nil {
			return err
		}
		if err := storageFailureRecoveryHost(recovery, p, binding); err != nil {
			return err
		}
		profile, err := loopruntime.LoadOpenShellProfile(p.Profile.Path)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
		defer cancel()
		receipt, err := loopruntime.StopStorageFailure(ctx, recovery.Stop, profile)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"status": "compute-stopped", "receipt": receipt, "task_outcome": "failed", "workspace_return": "abandoned", "original_execution_outcome": "unchanged"})
	}}
	stop.Flags().StringVar(&plan, "plan", "", "Original external recovery plan")
	stop.Flags().StringVar(&expected, "expected-sha256", "", "Reviewed exact recovery plan digest")
	_ = stop.MarkFlagRequired("plan")
	_ = stop.MarkFlagRequired("expected-sha256")
	root.AddCommand(stop)
	var inventoryPlan, inventoryExpected string
	inventory := &cobra.Command{Use: "inventory", Short: "Freshly verify every original native sandbox is stopped", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var recovery storageFailureRecovery
		ref, err := loopruntime.ReadSessionReadbackFile(inventoryPlan, &recovery)
		if err != nil {
			return err
		}
		if ref.SHA256 != inventoryExpected || recovery.Schema != 1 {
			return fmt.Errorf("storage failure recovery changed")
		}
		var p loopruntime.StorageFailureStopPlan
		if err = storageFailureRef(recovery.Stop, &p); err != nil {
			return err
		}
		profile, err := loopruntime.LoadOpenShellProfile(p.Profile.Path)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		observed, err := loopruntime.ObserveStorageFailureInventory(ctx, recovery.Stop, profile)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"status": "all-compute-stopped", "inventory": observed, "effects": []string{}})
	}}
	inventory.Flags().StringVar(&inventoryPlan, "plan", "", "Original external recovery plan")
	inventory.Flags().StringVar(&inventoryExpected, "expected-sha256", "", "Reviewed exact recovery plan digest")
	_ = inventory.MarkFlagRequired("plan")
	_ = inventory.MarkFlagRequired("expected-sha256")
	root.AddCommand(inventory)
	return root
}

func storageFailureRef(ref loopruntime.SessionReadbackFileRef, value any) error {
	actual, err := loopruntime.ReadSessionReadbackFile(ref.Path, value)
	if err != nil {
		return err
	}
	if actual != ref {
		return fmt.Errorf("storage failure source changed: %s", ref.Path)
	}
	return nil
}

type storageFailureHostPacket struct {
	Schema    int    `json:"schema"`
	Protocol  string `json:"protocol"`
	Instance  string `json:"host_instance_id"`
	PID       int    `json:"host_pid"`
	Binary    string `json:"binary_sha256"`
	Directory string `json:"host_dir"`
	Contexts  int    `json:"contexts"`
	Shutdown  struct {
		Requested bool `json:"requested"`
	} `json:"shutdown"`
	Residents struct {
		Records []admission.Resident `json:"records"`
		Error   json.RawMessage      `json:"error"`
	} `json:"residents"`
	Resources struct {
		Workers     int `json:"active_workers"`
		Residents   int `json:"active_residents"`
		Allocations int `json:"active_allocations"`
	} `json:"resources"`
}

func storageFailureHostIdentity(packet storageFailureHostPacket, p loopruntime.StorageFailureStopPlan, binding hostRequestBinding, rows []hostReceipt) error {
	if packet.Schema != 1 || packet.Protocol != hostProtocol || packet.Instance != p.HostInstance || packet.PID != p.HostPID || packet.Directory != binding.HostDir || packet.Binary != binding.BinarySHA256 || packet.Shutdown.Requested || packet.Contexts < 1 || packet.Resources.Workers != 1 || packet.Resources.Residents != 1 || packet.Resources.Allocations != 2 {
		return fmt.Errorf("storage failure original Host identity or resource ownership differs")
	}
	if len(packet.Residents.Error) == 0 || string(packet.Residents.Error) == "null" || len(packet.Residents.Records) == 0 {
		return fmt.Errorf("storage failure original resident persistence fault is unavailable")
	}
	active, matched := 0, 0
	for _, row := range rows {
		if row.ContextState == "detached" {
			continue
		}
		active++
		if row.HostInstanceID != p.HostInstance || row.HostPID != p.HostPID || row.HostDir != binding.HostDir || row.BinarySHA256 != binding.BinarySHA256 {
			return fmt.Errorf("storage failure retained Host history differs")
		}
		if row.ContextID == p.ContextID && row.RequestID == binding.RequestID && row.ProjectDir == binding.ProjectDir && row.ConfigSHA256 == binding.ConfigSHA256 && row.ConfigPath == binding.ConfigPath {
			matched++
		}
	}
	if active != packet.Contexts || matched != 1 {
		return fmt.Errorf("storage failure original context history is incomplete")
	}
	for _, r := range packet.Residents.Records {
		if r.State == "exited" && len(r.Contexts) == 0 {
			continue
		}
		if r.State != "running" || r.PID <= 1 || r.ProcessGroup != r.PID || r.Started == "" || r.GuardID == "" {
			return fmt.Errorf("storage failure resident generation is unresolved")
		}
		for _, contextID := range r.Contexts {
			found := 0
			for _, row := range rows {
				if row.ContextID == contextID && row.ContextState != "detached" {
					found++
				}
			}
			if found != 1 {
				return fmt.Errorf("storage failure resident has an unbound context")
			}
		}
	}
	return nil
}

func storageFailureRecoveryHost(recovery storageFailureRecovery, p loopruntime.StorageFailureStopPlan, binding hostRequestBinding) error {
	if err := loopruntime.VerifySessionReadbackFiles(recovery.Originals); err != nil {
		return err
	}
	var rows []hostReceipt
	if err := storageFailureRef(recovery.HostHistory, &rows); err != nil {
		return err
	}
	rows, err := storageFailureCurrentRows(binding.HostDir, rows)
	if err != nil {
		return err
	}
	var before storageFailureHostPacket
	if err := storageFailureRef(recovery.HostResources, &before); err != nil {
		return err
	}
	packet, err := progressMaintenanceResources(binding.HostDir)
	if err != nil {
		return err
	}
	data, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	var fresh storageFailureHostPacket
	if err = json.Unmarshal(data, &fresh); err != nil {
		return err
	}
	if err = storageFailureHostIdentity(fresh, p, binding, rows); err != nil {
		return err
	}
	if !sameStorageFailureResidents(before.Residents.Records, fresh.Residents.Records) {
		return fmt.Errorf("storage failure original resident membership changed")
	}
	if _, err = storageFailureBlockedHost(binding, p); err != nil {
		return err
	}
	return loopruntime.ValidateProgressMaintenanceLauncher(loopruntime.ProgressMaintenance{Launcher: recovery.Launcher, LauncherCall: recovery.LauncherCall, LauncherSession: recovery.LauncherSession})
}

// This is the original server's read-only status operation. Its sticky unknown
// context fence prevents another admission while the old generation is retired.
func storageFailureBlockedHost(binding hostRequestBinding, p loopruntime.StorageFailureStopPlan) (hostResponse, error) {
	var response hostResponse
	conn, err := net.DialTimeout("unix", filepath.Join(binding.HostDir, "host.sock"), time.Second)
	if err != nil {
		return response, err
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return response, err
	}
	req := hostRequest{Operation: "status", ProjectDir: binding.ProjectDir, InstanceID: p.HostInstance, RequiredHostInstanceID: p.HostInstance, ContextID: p.ContextID, RequestID: binding.RequestID, ConfigPath: binding.ConfigPath, ConfigSHA256: binding.ConfigSHA256, Mode: binding.Mode, CleanupCompleted: binding.CleanupCompleted, BinarySHA256: binding.BinarySHA256}
	if err = json.NewEncoder(conn).Encode(req); err != nil {
		return response, err
	}
	if err = json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&response); err != nil {
		return response, err
	}
	if response.Status != "unknown_effect" || response.Schema != 1 || response.Protocol != hostProtocol || response.HostPID != p.HostPID || response.HostInstanceID != p.HostInstance || response.ContextID != p.ContextID || response.RequestID != binding.RequestID || response.BinarySHA256 != binding.BinarySHA256 || response.HostDir != binding.HostDir || response.ProjectDir != binding.ProjectDir || response.ConfigPath != binding.ConfigPath || response.ConfigSHA256 != binding.ConfigSHA256 {
		return response, fmt.Errorf("storage failure original Host is not fenced by its unresolved context")
	}
	return response, nil
}

func storageFailureCurrentRows(directory string, rows []hostReceipt) ([]hostReceipt, error) {
	retired, err := verifySuccessorRetirements(directory, rows)
	if err != nil {
		return nil, err
	}
	current := make([]hostReceipt, 0, len(rows))
	for _, row := range rows {
		if !retired[row.ContextID] {
			current = append(current, row)
		}
	}
	return current, nil
}

func sameStorageFailureResidents(a, b []admission.Resident) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func prepareStorageFailure(request, session, output, implementation, launcher, launcherCall string, launcherSession int) (loopruntime.SessionReadbackFileRef, error) {
	var zero loopruntime.SessionReadbackFileRef
	var binding hostRequestBinding
	requestRef, err := loopruntime.ReadSessionReadbackFile(request, &binding)
	if err != nil {
		return zero, err
	}
	if binding.Schema != 1 || binding.Status != "accepted" || binding.RequiredHostInstanceID == "" || !filepath.IsAbs(binding.ProjectDir) || !filepath.IsAbs(output) || filepath.Base(session) != session || session == "." || session == ".." || launcherSession <= 0 {
		return zero, fmt.Errorf("storage failure exact original request or external path is invalid")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return zero, err
	}
	rel, err := filepath.Rel(binding.ProjectDir, parent)
	if err != nil || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return zero, fmt.Errorf("storage failure evidence directory is inside the original project")
	}
	data, err := readHostRequestConfig(hostRequest{ProjectDir: binding.ProjectDir, ConfigPath: binding.ConfigPath})
	if err != nil {
		return zero, err
	}
	if hostDigest(data) != binding.ConfigSHA256 {
		return zero, fmt.Errorf("storage failure original configuration changed")
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return zero, err
	}
	profile, err := loopruntime.LoadOpenShellProfile(cfg.Runtime.OpenShell.Profile)
	if err != nil {
		return zero, err
	}
	p := loopruntime.StorageFailureStopPlan{Schema: 1, Request: requestRef, SessionDirectory: filepath.Join(binding.ProjectDir, ".noodle", "sessions", session), HostInstance: binding.RequiredHostInstanceID}
	r := storageFailureRecovery{Schema: 1, LauncherSession: launcherSession, Originals: map[string]loopruntime.SessionReadbackFileRef{}}
	r.Launcher, err = loopruntime.ReadSessionReadbackFile(launcher, nil)
	if err != nil {
		return zero, err
	}
	r.LauncherCall, err = loopruntime.ReadSessionReadbackFile(launcherCall, nil)
	if err != nil {
		return zero, err
	}
	var rows []hostReceipt
	historyPath := filepath.Join(binding.HostDir, "receipts.json")
	historyRef, err := loopruntime.ReadSessionReadbackFile(historyPath, &rows)
	if err != nil {
		return zero, err
	}
	for _, row := range rows {
		if row.RequestID == binding.RequestID {
			p.ContextID, p.HostPID = row.ContextID, row.HostPID
		}
	}
	packet, err := progressMaintenanceResources(binding.HostDir)
	if err != nil {
		return zero, err
	}
	packetData, err := json.Marshal(packet)
	if err != nil {
		return zero, err
	}
	var observed storageFailureHostPacket
	if err = json.Unmarshal(packetData, &observed); err != nil {
		return zero, err
	}
	currentRows, err := storageFailureCurrentRows(binding.HostDir, rows)
	if err != nil {
		return zero, err
	}
	if err = storageFailureHostIdentity(observed, p, binding, currentRows); err != nil {
		return zero, err
	}
	for key, path := range map[string]string{"request": request, "config": binding.ConfigPath, "profile": profile.SourcePath, "dispatch": filepath.Join(p.SessionDirectory, "openshell-dispatch.json"), "custody": filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json"), "failure": filepath.Join(p.SessionDirectory, "progress-stop-output.log"), "native-implementation": implementation, "host-history": historyPath, "launcher": launcher, "launcher-call": launcherCall} {
		ref, readErr := loopruntime.ReadSessionReadbackFile(path, nil)
		if readErr != nil {
			return zero, readErr
		}
		r.Originals[key] = ref
	}
	p.Profile, p.Dispatch, p.Custody, p.Failure, p.NativeImplementation = r.Originals["profile"], r.Originals["dispatch"], r.Originals["custody"], r.Originals["failure"], r.Originals["native-implementation"]
	var dispatch struct{ OrderID string }
	if err = storageFailureRef(p.Dispatch, &dispatch); err != nil {
		return zero, err
	}
	if cfg.Mode != "manual" || binding.Mode != "manual" || len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.AdmittedOrders[0] != dispatch.OrderID {
		return zero, fmt.Errorf("storage failure original task is not the sole manually admitted order")
	}
	status, err := storageFailureBlockedHost(binding, p)
	if err != nil {
		return zero, err
	}
	if err = os.Mkdir(output, 0700); err != nil {
		return zero, err
	}
	p.Originals = map[string]loopruntime.SessionReadbackFileRef{}
	for key, ref := range r.Originals {
		bytes, readErr := os.ReadFile(ref.Path)
		if readErr != nil {
			return zero, readErr
		}
		if err = writeMaintenanceExclusive(filepath.Join(output, "original-"+key), bytes); err != nil {
			return zero, err
		}
		if key == "profile" || key == "dispatch" || key == "custody" || key == "failure" || key == "native-implementation" || key == "request" {
			copyRef, readErr := loopruntime.ReadSessionReadbackFile(filepath.Join(output, "original-"+key), nil)
			if readErr != nil {
				return zero, readErr
			}
			if key == "native-implementation" {
				key = "native_implementation"
			}
			p.Originals[key] = copyRef
		}
	}
	if err = loopruntime.ValidateStorageFailureStop(p, profile); err != nil {
		return zero, err
	}
	statusData, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return zero, err
	}
	if err = writeMaintenanceExclusive(filepath.Join(output, "host-status-before.json"), statusData); err != nil {
		return zero, err
	}
	r.HostStatus, err = loopruntime.ReadSessionReadbackFile(filepath.Join(output, "host-status-before.json"), nil)
	if err != nil {
		return zero, err
	}
	if err = writeMaintenanceExclusive(filepath.Join(output, "host-resources-before.json"), packetData); err != nil {
		return zero, err
	}
	r.HostResources, err = loopruntime.ReadSessionReadbackFile(filepath.Join(output, "host-resources-before.json"), nil)
	if err != nil {
		return zero, err
	}
	// The immutable copy is historical; the live original remains independently bound.
	r.HostHistory, err = loopruntime.ReadSessionReadbackFile(filepath.Join(output, "original-host-history"), nil)
	if err != nil {
		return zero, err
	}
	if r.HostHistory.SHA256 != historyRef.SHA256 {
		return zero, fmt.Errorf("storage failure Host history changed during preservation")
	}
	stopData, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return zero, err
	}
	if err = writeMaintenanceExclusive(filepath.Join(output, "stop-plan.json"), stopData); err != nil {
		return zero, err
	}
	r.Stop, err = loopruntime.ReadSessionReadbackFile(filepath.Join(output, "stop-plan.json"), nil)
	if err != nil {
		return zero, err
	}
	if err = storageFailureRecoveryHost(r, p, binding); err != nil {
		return zero, err
	}
	result, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return zero, err
	}
	if err = writeMaintenanceExclusive(filepath.Join(output, "recovery-plan.json"), result); err != nil {
		return zero, err
	}
	return loopruntime.ReadSessionReadbackFile(filepath.Join(output, "recovery-plan.json"), nil)
}
