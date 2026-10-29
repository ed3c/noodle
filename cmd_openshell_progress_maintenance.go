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
	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

func newProgressMaintenanceCmd() *cobra.Command {
	root := &cobra.Command{Use: "progress-maintenance", Short: "Recover one explicitly drained OpenShell attempt without replaying its model"}
	var request, session, archives, output, plan, expected, launcher, launcherCall string
	var launcherSession int
	var repairPath, repairSHA string
	var historyPath, historySHA string
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ref, err := prepareProgressMaintenance(request, session, archives, output, launcher, launcherCall, launcherSession)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"status": "prepared", "plan": ref, "effects": []string{}})
	}}
	for name, target := range map[string]*string{"request-file": &request, "session-id": &session, "archives": &archives, "output": &output} {
		prepare.Flags().StringVar(target, name, "", "Exact original input or new external output directory")
		_ = prepare.MarkFlagRequired(name)
	}
	root.AddCommand(prepare)
	prepare.Flags().StringVar(&launcher, "launcher-intent", "", "Original Host launcher intent")
	prepare.Flags().IntVar(&launcherSession, "launcher-session", 0, "Original launcher execution session identifier")
	prepare.Flags().StringVar(&launcherCall, "launcher-call", "", "Preserved original launcher tool call and return")
	_ = prepare.MarkFlagRequired("launcher-intent")
	_ = prepare.MarkFlagRequired("launcher-session")
	_ = prepare.MarkFlagRequired("launcher-call")
	for _, phase := range []string{"stop", "release", "release-inspect", "restore", "restore-inspect", "restore-continue"} {
		child := &cobra.Command{Use: phase, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			var p loopruntime.ProgressMaintenance
			ref, err := loopruntime.ReadSessionReadbackFile(plan, &p)
			if err != nil {
				return err
			}
			if ref.SHA256 != expected {
				return fmt.Errorf("reviewed maintenance plan changed")
			}
			profile, err := loopruntime.LoadOpenShellProfile(p.Profile.Path)
			if err != nil {
				return err
			}
			var result loopruntime.SessionReadbackFileRef
			if phase == "stop" {
				controlLock, lockErr := lockfile.TryLock(filepath.Join(filepath.Dir(filepath.Dir(p.SessionDirectory)), "control.lock"))
				if lockErr != nil {
					return lockErr
				}
				defer controlLock.Close()
				if err = progressMaintenanceHost(p); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
				defer cancel()
				result, err = loopruntime.StopProgressMaintenance(ctx, ref, profile)
			} else if phase == "release" || phase == "release-inspect" {
				ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
				defer cancel()
				result, err = releaseProgressMaintenance(ctx, ref, p, profile, phase == "release-inspect")
			} else {
				var repair *loopruntime.SessionReadbackFileRef
				var history loopruntime.SessionReadbackFileRef
				if phase == "restore-inspect" {
					history, err = loopruntime.ReadSessionReadbackFile(historyPath, nil)
					if err != nil {
						return err
					}
					if history.SHA256 != historySHA {
						return fmt.Errorf("reviewed original Host history changed")
					}
				}
				if phase == "restore-continue" {
					bound, readErr := loopruntime.ReadSessionReadbackFile(repairPath, nil)
					if readErr != nil {
						return readErr
					}
					if bound.SHA256 != repairSHA {
						return fmt.Errorf("reviewed restore repair changed")
					}
					repair = &bound
				}
				result, err = restoreProgressMaintenance(cmd.Context(), ref, p, profile, phase, repair, history)
			}
			if err != nil {
				return err
			}
			response := map[string]any{"status": phase + "-complete", "receipt": result}
			if phase == "restore-inspect" {
				executable, err := os.Executable()
				if err != nil {
					return err
				}
				response["next"] = map[string]any{"owner": "Noodle", "argv": []string{executable, "openshell", "progress-maintenance", "restore-continue", "--plan", ref.Path, "--expected-sha256", ref.SHA256, "--repair-file", result.Path, "--expected-repair-sha256", result.SHA256}}
				response["effects"] = []string{}
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
		}}
		child.Flags().StringVar(&plan, "plan", "", "Original immutable maintenance plan")
		child.Flags().StringVar(&expected, "expected-sha256", "", "Reviewed exact plan digest")
		_ = child.MarkFlagRequired("plan")
		_ = child.MarkFlagRequired("expected-sha256")
		if phase == "restore-inspect" {
			child.Flags().StringVar(&historyPath, "host-history-file", "", "Independently retained original native Host responses")
			child.Flags().StringVar(&historySHA, "expected-host-history-sha256", "", "Reviewed exact original Host history digest")
			_ = child.MarkFlagRequired("host-history-file")
			_ = child.MarkFlagRequired("expected-host-history-sha256")
		}
		if phase == "restore-continue" {
			child.Flags().StringVar(&repairPath, "repair-file", "", "Original pre-publication inspection packet")
			child.Flags().StringVar(&repairSHA, "expected-repair-sha256", "", "Reviewed exact repair packet digest")
			_ = child.MarkFlagRequired("repair-file")
			_ = child.MarkFlagRequired("expected-repair-sha256")
		}
		root.AddCommand(child)
	}
	return root
}

func progressMaintenanceResources(dir string) (map[string]json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(hostRequest{Operation: "resources"}); err != nil {
		return nil, err
	}
	var packet map[string]json.RawMessage
	err = json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&packet)
	return packet, err
}

func progressMaintenanceHost(p loopruntime.ProgressMaintenance) error {
	packet, err := progressMaintenanceResources(p.HostDir)
	if err != nil {
		return err
	}
	var schema int
	var protocol, hostDir string
	if err = json.Unmarshal(packet["schema"], &schema); err != nil {
		return err
	}
	if err = json.Unmarshal(packet["protocol"], &protocol); err != nil {
		return err
	}
	if err = json.Unmarshal(packet["host_dir"], &hostDir); err != nil {
		return err
	}
	if schema != 1 || protocol != hostProtocol || hostDir != p.HostDir {
		return fmt.Errorf("maintenance native Host protocol or directory differs")
	}
	var instance string
	var pid, contexts int
	if err = json.Unmarshal(packet["host_instance_id"], &instance); err != nil {
		return err
	}
	if err = json.Unmarshal(packet["host_pid"], &pid); err != nil {
		return err
	}
	if err = json.Unmarshal(packet["contexts"], &contexts); err != nil {
		return err
	}
	var shutdown struct{ Requested *bool }
	if err = json.Unmarshal(packet["shutdown"], &shutdown); err != nil {
		return err
	}
	var resources struct {
		Workers     int  `json:"active_workers"`
		Residents   int  `json:"active_residents"`
		Allocations int  `json:"active_allocations"`
		Unknown     *int `json:"unresolved_residents"`
	}
	if err = json.Unmarshal(packet["resources"], &resources); err != nil {
		return err
	}
	if instance != p.HostInstance || pid != p.HostPID || contexts != 1 || shutdown.Requested == nil || *shutdown.Requested || resources.Workers != 1 || resources.Residents != 1 || resources.Allocations != 2 || resources.Unknown == nil || *resources.Unknown != 0 {
		return fmt.Errorf("maintenance original Host generation or sole task allocation changed")
	}
	var binding hostRequestBinding
	requestRef, err := loopruntime.ReadSessionReadbackFile(p.Request.Path, &binding)
	if err != nil {
		return err
	}
	if requestRef != p.Request || binding.RequiredHostInstanceID != instance || binding.HostDir != p.HostDir || filepath.Join(binding.ProjectDir, ".noodle", "sessions", filepath.Base(p.SessionDirectory)) != p.SessionDirectory {
		return fmt.Errorf("maintenance original request differs")
	}
	var binary string
	if err = json.Unmarshal(packet["binary_sha256"], &binary); err != nil {
		return err
	}
	if binary != binding.BinarySHA256 {
		return fmt.Errorf("maintenance original Host binary changed")
	}
	var receipts []hostReceipt
	if _, err = loopruntime.ReadSessionReadbackFile(filepath.Join(p.HostDir, "receipts.json"), &receipts); err != nil {
		return err
	}
	matches := 0
	for _, r := range receipts {
		if r.RequestID == binding.RequestID && r.HostInstanceID == p.HostInstance && r.HostPID == p.HostPID && r.ContextID == p.ContextID && r.ContextState == "active" && r.BinarySHA256 == binary && r.ConfigSHA256 == binding.ConfigSHA256 && r.ConfigPath == binding.ConfigPath && r.ProjectDir == binding.ProjectDir && r.HostDir == binding.HostDir && r.Mode == binding.Mode && r.CleanupCompleted == binding.CleanupCompleted {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("maintenance original context binding changed")
	}
	if err := maintenanceResidentUnchanged(p, packet); err != nil {
		return err
	}
	if err := maintenanceOriginalAttempt(p); err != nil {
		return err
	}
	statusBytes, err := os.ReadFile(filepath.Join(binding.ProjectDir, ".noodle", "status.json"))
	if err != nil {
		return err
	}
	var status struct {
		LoopState string `json:"loop_state"`
	}
	if err = json.Unmarshal(statusBytes, &status); err != nil {
		return err
	}
	if status.LoopState != "paused" {
		return fmt.Errorf("maintenance original Loop is not paused")
	}
	for _, name := range []string{"control.ndjson", "orders-next.json"} {
		data, err := os.ReadFile(filepath.Join(binding.ProjectDir, ".noodle", name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if len(data) != 0 {
			return fmt.Errorf("maintenance original Loop has pending input: %s", name)
		}
	}
	return nil
}

func prepareProgressMaintenance(request, session, archives, output, launcher, launcherCall string, launcherSession int) (loopruntime.SessionReadbackFileRef, error) {
	var p loopruntime.ProgressMaintenance
	var binding hostRequestBinding
	ref, err := loopruntime.ReadSessionReadbackFile(request, &binding)
	if err != nil {
		return ref, err
	}
	if binding.Schema != 1 || binding.Status != "accepted" || binding.RequiredHostInstanceID == "" || !filepath.IsAbs(binding.ConfigPath) || filepath.Base(session) != session || session == "." || session == ".." || !filepath.IsAbs(output) || !filepath.IsAbs(archives) || launcherSession <= 0 {
		return ref, fmt.Errorf("maintenance original request or external path invalid")
	}
	data, err := readHostRequestConfig(hostRequest{ProjectDir: binding.ProjectDir, ConfigPath: binding.ConfigPath})
	if err != nil {
		return ref, err
	}
	if hostDigest(data) != binding.ConfigSHA256 {
		return ref, fmt.Errorf("maintenance original configuration changed")
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return ref, err
	}
	profile, err := loopruntime.LoadOpenShellProfile(cfg.Runtime.OpenShell.Profile)
	if err != nil {
		return ref, err
	}
	for _, candidate := range []string{archives, filepath.Dir(output)} {
		canonical, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return ref, err
		}
		relative, err := filepath.Rel(binding.ProjectDir, canonical)
		if err != nil || relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return ref, fmt.Errorf("maintenance evidence path is inside the original project")
		}
	}
	var receipts []hostReceipt
	if _, err = loopruntime.ReadSessionReadbackFile(filepath.Join(binding.HostDir, "receipts.json"), &receipts); err != nil {
		return ref, err
	}
	var original hostReceipt
	count := 0
	for _, r := range receipts {
		if r.RequestID == binding.RequestID {
			original = r
			count++
		}
	}
	if count != 1 || original.ContextState != "active" || original.HostInstanceID != binding.RequiredHostInstanceID || original.ConfigSHA256 != binding.ConfigSHA256 || original.BinarySHA256 != binding.BinarySHA256 {
		return ref, fmt.Errorf("maintenance original active context differs")
	}
	p = loopruntime.ProgressMaintenance{Schema: 1, HostDir: binding.HostDir, HostInstance: original.HostInstanceID, HostPID: original.HostPID, ContextID: original.ContextID, Request: ref, SessionDirectory: filepath.Join(binding.ProjectDir, ".noodle", "sessions", session), Sources: map[string]loopruntime.SessionReadbackFileRef{}, Originals: map[string]loopruntime.SessionReadbackFileRef{}, Archives: map[string]loopruntime.SessionReadbackFileRef{}}
	p.Profile, err = loopruntime.ReadSessionReadbackFile(profile.SourcePath, nil)
	if err != nil {
		return ref, err
	}
	packet, err := progressMaintenanceResources(p.HostDir)
	if err != nil {
		return ref, err
	}
	p.Resident, err = maintenanceResident(packet, p, profile.Gateway.Resident.Name)
	if err != nil {
		return ref, err
	}
	p.LauncherSession = launcherSession
	var launch struct {
		Argv         []string          `json:"argv"`
		Environment  map[string]string `json:"environment"`
		BinarySHA256 string            `json:"binary_sha256"`
	}
	p.Launcher, err = loopruntime.ReadSessionReadbackFile(launcher, &launch)
	if err != nil {
		return ref, err
	}
	p.LauncherCall, err = loopruntime.ReadSessionReadbackFile(launcherCall, nil)
	if err != nil {
		return ref, err
	}
	if err := loopruntime.ValidateProgressMaintenanceLauncher(p); err != nil {
		return ref, err
	}
	if len(launch.Argv) != 3 || launch.Argv[0] != original.BinaryPath || launch.Argv[1] != "host" || launch.Argv[2] != "serve" || launch.BinarySHA256 != original.BinarySHA256 || launch.Environment["NOODLE_HOST_DIR"] != p.HostDir {
		return ref, fmt.Errorf("maintenance original launcher differs")
	}
	binaryRef, err := loopruntime.ReadSessionReadbackFile(original.BinaryPath, nil)
	if err != nil {
		return ref, err
	}
	if binaryRef.SHA256 != original.BinarySHA256 {
		return ref, fmt.Errorf("maintenance original Host executable changed")
	}
	p.Sources["original-binary"], p.Sources["launcher"] = binaryRef, p.Launcher
	p.Sources["launcher-call"] = p.LauncherCall
	if err = progressMaintenanceHost(p); err != nil {
		return ref, err
	}
	for key, name := range map[string]string{"config": binding.ConfigPath, "dispatch": filepath.Join(p.SessionDirectory, "openshell-dispatch.json"), "custody": filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json"), "spawn": filepath.Join(p.SessionDirectory, "spawn.json"), "input": filepath.Join(p.SessionDirectory, "input.txt"), "stop-output": filepath.Join(p.SessionDirectory, "progress-stop-output.log")} {
		r, err := loopruntime.ReadSessionReadbackFile(name, nil)
		if err != nil {
			return ref, err
		}
		p.Sources[key] = r
	}
	var dispatch struct{ OrderID string }
	if _, err = loopruntime.ReadSessionReadbackFile(p.Sources["dispatch"].Path, &dispatch); err != nil {
		return ref, err
	}
	if len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.AdmittedOrders[0] != dispatch.OrderID {
		return ref, fmt.Errorf("maintenance task differs from the sole admitted order")
	}
	for _, name := range []string{"result.json", "noodle-result.tar", "noodle-session.tar"} {
		r, err := loopruntime.ReadSessionReadbackFile(filepath.Join(archives, name), nil)
		if err != nil {
			return ref, err
		}
		p.Archives[name] = r
	}
	if err = loopruntime.ValidateProgressMaintenance(p, profile); err != nil {
		return ref, err
	}
	if err = os.Mkdir(output, 0700); err != nil {
		return ref, err
	}
	for key, source := range p.Sources {
		if key == "original-binary" {
			p.Originals[key] = source
			continue
		}
		bytes, err := os.ReadFile(source.Path)
		if err != nil {
			return ref, err
		}
		dest := filepath.Join(output, "original-"+key+".json")
		if err = os.WriteFile(dest, bytes, 0600); err != nil {
			return ref, err
		}
		p.Originals[key], err = loopruntime.ReadSessionReadbackFile(dest, nil)
		if err != nil {
			return ref, err
		}
	}
	if err = progressMaintenanceHost(p); err != nil {
		return ref, err
	}
	return loopruntime.WriteProgressMaintenance(filepath.Join(output, "plan.json"), p, profile)
}
