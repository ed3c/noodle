package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/guestprogress"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

func maintenanceFixture(t *testing.T) (*OpenShellRuntime, *managedFixture, ProgressMaintenance, SessionReadbackFileRef) {
	t.Helper()
	r, f, target := openShellRuntimeFixture(t)
	managedMarker(t, f.root, "completion-observation")
	profileFile := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(profileFile, []byte("frozen test profile"), 0600); err != nil {
		t.Fatal(err)
	}
	profileRef, err := ReadSessionReadbackFile(profileFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.config.Profile.SourcePath, r.config.Profile.SourceSHA256 = profileRef.Path, profileRef.SHA256
	r.config.Profile.sourceFiles = map[string]string{profileRef.Path: profileRef.SHA256}
	c, err := f.owner.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	r.config.Profile.Gateway.Binding = c.state.Binding
	record := custodyCreate(t, c, "maintenance")
	session := record.Task.SessionID
	dir := filepath.Join(r.config.RuntimeDir, "sessions", session)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	base := openShellTestCommand()
	base.Argv = []string{r.config.Profile.Routes["codex"].Binary}
	base.Workdir = "/sandbox/repo"
	base.Env = map[string]string{"NOODLE_SESSION_DIR": "/sandbox/noodle-sessions/" + session}
	command, err := wrapOpenShellProgressIn(base, r.config.Profile.ProgressSupervisor, session, strings.Repeat("a", 64), path.Join("/tmp", path.Base(progressDirectory(session))))
	if err != nil {
		t.Fatal(err)
	}
	control, err := progressStopCommand(command, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state.Records[0].Exec = &OpenShellCustodyExec{AttemptID: record.Task.AttemptID, Command: command, Result: OpenShellExecResult{RequestID: command.RequestID, StartAttempted: true, ExitObserved: true}}
	c.state.Records[0].ProgressStop = &OpenShellCustodyExec{AttemptID: record.Task.AttemptID + "/progress-stop", Command: control, Result: OpenShellExecResult{RequestID: control.RequestID, StartAttempted: true, ExitObserved: true}}
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	record = c.state.Records[0]
	c.mu.Unlock()
	_, binding, err := unwrapOpenShellProgress(command)
	if err != nil {
		t.Fatal(err)
	}
	token := sha256.Sum256([]byte(binding.Token))
	saved := guestprogress.Receipt{Schema: 1, Identity: guestprogress.Identity{SessionID: session, CommandSHA256: binding.CommandSHA256, TokenSHA256: hex.EncodeToString(token[:])}, RequestID: control.RequestID, Status: "archived", ExitCode: 143, ChildrenReaped: true}
	archiveDir := t.TempDir()
	archives := map[string]SessionReadbackFileRef{}
	for _, name := range []string{"noodle-result.tar", "noodle-session.tar"} {
		data := progressFixtureArchive(t, name)
		file := filepath.Join(archiveDir, name)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := ReadSessionReadbackFile(file, nil)
		if err != nil {
			t.Fatal(err)
		}
		archives[name] = ref
		saved.Archives = append(saved.Archives, guestprogress.Archive{Name: name, SHA256: ref.SHA256, Bytes: int64(len(data))})
	}
	if err := maintenanceWrite(filepath.Join(archiveDir, "result.json"), saved); err != nil {
		t.Fatal(err)
	}
	archives["result.json"], err = ReadSessionReadbackFile(filepath.Join(archiveDir, "result.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := workspaceartifacts.Capture(t.Context(), target, filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	profileSHA, err := OpenShellDispatchSHA256(r.config.Profile)
	if err != nil {
		t.Fatal(err)
	}
	d := openShellDispatchReceipt{Version: 1, ProfileSHA256: profileSHA, ContextID: "a", OrderID: record.Task.OrderID, SessionID: session, AttemptID: record.Task.AttemptID, Target: target, Phase: "progress-unknown", Error: "native workspace download refused", Baseline: baseline, Worker: &record.Worker, Outcome: SessionOutcome{Status: StatusFailed, Reason: "native workspace download refused", ExitCode: -1}}
	if err := maintenanceWrite(filepath.Join(dir, "openshell-dispatch.json"), d); err != nil {
		t.Fatal(err)
	}
	if err := maintenanceWrite(filepath.Join(dir, "progress-stop-output.log"), saved); err != nil {
		t.Fatal(err)
	}
	if err := maintenanceWrite(filepath.Join(dir, "spawn.json"), map[string]any{"session_id": session, "provider": "codex", "model": "fixture", "runtime": "openshell", "worktree_path": target, "order_id": d.OrderID, "stage_index": "0"}); err != nil {
		t.Fatal(err)
	}
	if err := maintenanceWrite(filepath.Join(dir, "meta.json"), map[string]any{"session_id": session, "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "failed", "alive": false}); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(t.TempDir(), "native-request.json")
	if err := maintenanceWrite(request, map[string]any{"request_id": "original", "project_dir": r.config.RuntimeDir, "binary_sha256": "original-binary", "config_sha256": "original-config"}); err != nil {
		t.Fatal(err)
	}
	requestRef, err := ReadSessionReadbackFile(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := ProgressMaintenance{Schema: 1, HostDir: t.TempDir(), HostInstance: "original-host", HostPID: 99999999, ContextID: "a", Request: requestRef, Profile: profileRef, SessionDirectory: dir, Sources: map[string]SessionReadbackFileRef{}, Originals: map[string]SessionReadbackFileRef{}, Archives: archives}
	rows, err := f.residents.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	p.Resident = rows[0]
	p.Launcher, p.LauncherSession = profileRef, 19748
	launchOutput, err := json.Marshal(map[string]any{"intent": p.Launcher.Path, "launcher": filepath.Dir(p.Launcher.Path)})
	if err != nil {
		t.Fatal(err)
	}
	p.LauncherCall = maintenanceToolFixture(t, "text(await tools.exec_command({}));", map[string]any{"session_id": p.LauncherSession, "output": string(launchOutput)}, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour+time.Second))
	directory := t.TempDir()
	for key, file := range map[string]string{"dispatch": filepath.Join(dir, "openshell-dispatch.json"), "custody": filepath.Join(c.state.Binding.StateRoot, "owner.json"), "spawn": filepath.Join(dir, "spawn.json"), "stop-output": filepath.Join(dir, "progress-stop-output.log"), "launcher-call": p.LauncherCall.Path} {
		ref, err := ReadSessionReadbackFile(file, nil)
		if err != nil {
			t.Fatal(err)
		}
		p.Sources[key] = ref
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := filepath.Join(directory, "original-"+key+".json")
		if err := os.WriteFile(snapshot, data, 0600); err != nil {
			t.Fatal(err)
		}
		p.Originals[key], err = ReadSessionReadbackFile(snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	ref, err := WriteProgressMaintenance(filepath.Join(directory, "plan.json"), p, r.config.Profile)
	if err != nil {
		t.Fatal(err)
	}
	return r, f, p, ref
}

func TestProgressMaintenanceStopsOnceAndRestoresOriginalArchives(t *testing.T) {
	r, f, p, ref := maintenanceFixture(t)
	stop, err := StopProgressMaintenance(t.Context(), ref, r.config.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StopProgressMaintenance(t.Context(), ref, r.config.Profile); err == nil {
		t.Fatal("maintenance native stop replayed")
	}
	alternate, err := WriteProgressMaintenance(filepath.Join(t.TempDir(), "plan.json"), p, r.config.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StopProgressMaintenance(t.Context(), alternate, r.config.Profile); err == nil {
		t.Fatal("another plan directory replayed the original native stop")
	}
	finish := func(complete SessionReadbackFileRef) error { return maintenanceHostFixture(t, r, p, ref, complete) }
	if _, err := RestoreProgressMaintenance(t.Context(), ref, r.config.Profile, finish); err == nil {
		t.Fatal("live custody was taken over")
	}
	if err := f.owner.custody.closeLocal(); err != nil {
		t.Fatal(err)
	}
	complete, err := RestoreProgressMaintenance(t.Context(), ref, r.config.Profile, finish)
	if err != nil {
		t.Fatal(err)
	}
	var d openShellDispatchReceipt
	if _, err := ReadSessionReadbackFile(p.Sources["dispatch"].Path, &d); err != nil {
		t.Fatal(err)
	}
	if d.ProgressHead == "" || d.CandidateHead != "" || d.Outcome.Status != StatusCancelled || d.Maintenance == nil || *d.Maintenance != complete {
		t.Fatalf("recovery changed authority: %+v", d)
	}
	before := p.Originals["dispatch"]
	if _, err := maintenanceCompletion(d); err != nil {
		t.Fatal(err)
	}
	var original openShellDispatchReceipt
	if err := maintenanceVerify(before, &original); err != nil {
		t.Fatal(err)
	}
	if original.Error == "" || original.Phase != "progress-unknown" {
		t.Fatal("original failure was overwritten")
	}
	var proof ProgressMaintenanceStop
	if err := maintenanceVerify(stop, &proof); err != nil {
		t.Fatal(err)
	}
	c, err := OpenOpenShellCustody(t.Context(), r.config.Profile.Gateway.Binding, r.config.Profile.Gateway.TLS, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.owner.custody = c
	c.managed = true
	c.residentRunning = f.owner.generation.Running
	if err := validateProgressRecord(c.Snapshot().Records[0], d.Worker.Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Acquire(t.Context(), "b"); err != nil {
		t.Fatal(err)
	}
	r.config.ContextID = "b"
	readback, err := r.ReadSavedProgress(t.Context(), CompletedSessionBinding{OrderID: d.OrderID, SessionID: d.SessionID, WorktreePath: d.Target, Provider: "codex", Model: "fixture", StageIndex: 0, RetryCount: 0})
	if err != nil {
		t.Fatal(err)
	}
	if readback.ProgressHead != d.ProgressHead || !readback.StopAcknowledged || readback.NativePhase != "Stopped" {
		t.Fatalf("maintenance native readback differs: %+v", readback)
	}
	beforeRecovery, err := os.ReadFile(filepath.Join(f.root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := r.Recover(t.Context()); err != nil || len(recovered) != 0 {
		t.Fatalf("maintained history did not recover through its native owner: %v %v", recovered, err)
	}
	afterRecovery, err := os.ReadFile(filepath.Join(f.root, "events"))
	if err != nil || !strings.Contains(string(afterRecovery[len(beforeRecovery):]), "get\n") || strings.Contains(string(afterRecovery[len(beforeRecovery):]), "stop\n") {
		t.Fatal("native recovery skipped fresh readback or repeated Stop")
	}
	for _, kind := range []string{"missing-plan-intent", "foreign-session-reservation"} {
		t.Run(kind, func(t *testing.T) {
			name := filepath.Join(filepath.Dir(ref.Path), "stop-intent.json")
			if kind == "foreign-session-reservation" {
				name = filepath.Join(p.SessionDirectory, "maintenance-stop-intent.json")
			}
			original, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing-plan-intent" {
				err = os.Remove(name)
			} else {
				err = os.WriteFile(name, []byte(`{"RequestID":"foreign"}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := r.ReadSavedProgress(t.Context(), CompletedSessionBinding{OrderID: d.OrderID, SessionID: d.SessionID, WorktreePath: d.Target, Provider: "codex", Model: "fixture", StageIndex: 0, RetryCount: 0})
			_, recoverErr := r.Recover(t.Context())
			if err := os.WriteFile(name, original, 0600); err != nil {
				t.Fatal(err)
			}
			if readErr == nil || recoverErr == nil {
				t.Fatal("maintenance runtime accepted an incomplete Stop chain")
			}
		})
	}
	if _, err := f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Release("b"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Archives["result.json"].Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreProgressMaintenance(t.Context(), ref, r.config.Profile, finish); err == nil {
		t.Fatal("changed archive allowed restore replay")
	}
}

// Host-side documents here are disposable protocol fixtures. The separate CLI
// gate proves process/group absence; these documents make no physical claim.
func maintenanceHostFixture(t *testing.T, r *OpenShellRuntime, p ProgressMaintenance, ref, complete SessionReadbackFileRef) error {
	t.Helper()
	lock, err := lockfile.TryLock(filepath.Join(r.config.Profile.Gateway.Binding.StateRoot, "owner.lock"))
	if err == nil {
		lock.Close()
		t.Fatal("custody lock was released before Host handover")
	}
	var pending openShellDispatchReceipt
	if _, err := ReadSessionReadbackFile(p.Sources["dispatch"].Path, &pending); err != nil {
		return err
	}
	if _, err := maintenanceCompletion(pending); err == nil {
		t.Fatal("incomplete Host handover accepted")
	}
	dir := filepath.Dir(ref.Path)
	old := map[string]any{"request_id": "original", "project_dir": r.config.RuntimeDir, "binary_sha256": "original-binary", "config_sha256": "original-config", "host_instance_id": p.HostInstance, "host_pid": p.HostPID, "context_id": p.ContextID, "context_state": "failed"}
	beforePath, afterPath := filepath.Join(dir, "host-receipts-before.json"), filepath.Join(dir, "host-after.json")
	if _, err := os.Lstat(beforePath); os.IsNotExist(err) {
		if err := maintenanceWrite(beforePath, []map[string]any{old}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	old["context_state"], old["terminal_cause"] = "detached", "maintenance-handoff"
	if err := maintenanceWrite(afterPath, []map[string]any{old}); err != nil {
		return err
	}
	before, err := ReadSessionReadbackFile(beforePath, nil)
	if err != nil {
		return err
	}
	after, err := ReadSessionReadbackFile(afterPath, nil)
	if err != nil {
		return err
	}
	resident := p.Resident
	resident.State = "exited"
	resident.Contexts = nil
	resident.Released = []string{p.ContextID}
	clock := time.Now().UTC()
	resident.StopRequested = clock.Add(-3 * time.Second).Format(time.RFC3339Nano)
	if err := maintenanceWrite(filepath.Join(p.SessionDirectory, "maintenance-release-intent.json"), map[string]any{"plan": ref, "requested_at": clock.Add(-4 * time.Second), "request": map[string]any{"operation": "resident_release_bound", "instance_id": p.HostInstance, "context_id": p.ContextID, "request_id": "original", "resident_id": p.Resident.ID}}); err != nil {
		return err
	}
	intent, response, _, err := ProgressMaintenanceReleaseSources(ref, p)
	if err != nil {
		return err
	}
	resourcesPath := filepath.Join(dir, "released-resources.json")
	if err := maintenanceWrite(resourcesPath, map[string]any{"host_instance_id": p.HostInstance, "host_pid": p.HostPID, "host_dir": p.HostDir, "residents": map[string]any{"records": []any{resident}, "error": nil}}); err != nil {
		return err
	}
	resources, err := ReadSessionReadbackFile(resourcesPath, nil)
	if err != nil {
		return err
	}
	release := ProgressMaintenanceRelease{Plan: ref, Resources: resources, Intent: intent, Response: response, Resident: resident, ObservedAt: clock.Add(-2 * time.Second)}
	releasePath := filepath.Join(dir, "release-complete.json")
	if err := maintenanceWrite(releasePath, release); err != nil {
		return err
	}
	statePath := filepath.Join(dir, "fixture-wait.json")
	if err := maintenanceWrite(statePath, map[string]any{"exit_code": 0, "output": "disposable fixture waiter"}); err != nil {
		return err
	}
	state, err := ReadSessionReadbackFile(statePath, nil)
	if err != nil {
		return err
	}
	waitCall := maintenanceToolFixture(t, `text(await tools.write_stdin({"session_id":19748}));`, map[string]any{"exit_code": 0, "output": "disposable fixture waiter"}, clock.Add(-1500*time.Millisecond), clock.Add(-1250*time.Millisecond))
	exit := ProgressMaintenanceExit{Plan: ref, WaitResult: state, WaitCall: waitCall, HostInstance: p.HostInstance, HostPID: p.HostPID, LauncherSession: p.LauncherSession, ObservedAt: clock.Add(-time.Second)}
	exitPath := filepath.Join(dir, "host-exit.json")
	if err := maintenanceWrite(exitPath, exit); err != nil {
		return err
	}
	releaseRef, err := ReadSessionReadbackFile(releasePath, nil)
	if err != nil {
		return err
	}
	exitRef, err := ReadSessionReadbackFile(exitPath, nil)
	if err != nil {
		return err
	}
	h := ProgressMaintenanceHostHandoff{Schema: 1, Completion: complete, HostInstance: p.HostInstance, HostReceiptsBefore: before, HostReceiptsAfter: after, Release: releaseRef, HostExit: exitRef}
	return maintenanceWrite(filepath.Join(dir, "host-handoff.json"), h)
}

func TestProgressMaintenanceLostStopAcknowledgementNeverReplays(t *testing.T) {
	r, f, _, ref := maintenanceFixture(t)
	managedMarker(t, f.root, "stop-ack-lost")
	if _, err := StopProgressMaintenance(t.Context(), ref, r.config.Profile); err == nil {
		t.Fatal("lost stop acknowledgement accepted")
	}
	for range 2 {
		if _, err := StopProgressMaintenance(t.Context(), ref, r.config.Profile); err == nil {
			t.Fatal("unknown native stop replayed")
		}
	}
	data, err := os.ReadFile(filepath.Join(f.root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "stop\n") != 1 {
		t.Fatal("maintenance submitted more than one native stop")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ref.Path), "stop-complete.json")); !os.IsNotExist(err) {
		t.Fatal("unknown stop produced completion")
	}
	// The fixture alone may release its fake service after observing the effect;
	// the maintenance protocol still retains the missing ACK as unknown.
	if _, err := f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestProgressMaintenanceClosureRequiresReleaseBeforeOriginalLauncherExit(t *testing.T) {
	r, f, p, ref := maintenanceFixture(t)
	if _, err := StopProgressMaintenance(t.Context(), ref, r.config.Profile); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.custody.closeLocal(); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreProgressMaintenance(t.Context(), ref, r.config.Profile, func(complete SessionReadbackFileRef) error {
		return maintenanceHostFixture(t, r, p, ref, complete)
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(ref.Path)
	exitPath := filepath.Join(dir, "host-exit.json")
	var original ProgressMaintenanceExit
	if _, err := ReadSessionReadbackFile(exitPath, &original); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"valid", "before-release", "foreign-launcher", "foreign-host", "running-waiter", "missing-exit", "foreign-wait-call", "foreign-thread", "unrequested-exit", "refused-release"} {
		t.Run(kind, func(t *testing.T) {
			exit := original
			switch kind {
			case "before-release":
				exit.ObservedAt = time.Unix(1, 0)
			case "foreign-launcher":
				exit.LauncherSession++
			case "foreign-host":
				exit.HostInstance = "foreign"
			case "foreign-wait-call":
				exit.WaitCall = maintenanceToolFixture(t, `text(await tools.write_stdin({"session_id":19749}));`, map[string]any{"exit_code": 0, "output": "disposable fixture waiter"}, original.ObservedAt.Add(-500*time.Millisecond), original.ObservedAt.Add(-250*time.Millisecond))
			case "foreign-thread":
				data, err := os.ReadFile(original.WaitCall.Path)
				if err != nil {
					t.Fatal(err)
				}
				name := filepath.Join(t.TempDir(), "foreign-thread.json")
				if err := os.WriteFile(name, []byte(strings.ReplaceAll(string(data), "disposable-fixture", "foreign-thread")), 0600); err != nil {
					t.Fatal(err)
				}
				exit.WaitCall, err = ReadSessionReadbackFile(name, nil)
				if err != nil {
					t.Fatal(err)
				}
			case "unrequested-exit":
				name := filepath.Join(dir, "release-complete.json")
				before, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(name, before, 0600); err != nil {
						t.Error(err)
					}
				})
				var release ProgressMaintenanceRelease
				if err := json.Unmarshal(before, &release); err != nil {
					t.Fatal(err)
				}
				release.Resident.StopRequested = ""
				resourcesPath := filepath.Join(t.TempDir(), "unrequested-resources.json")
				if err := maintenanceWrite(resourcesPath, map[string]any{"host_instance_id": p.HostInstance, "host_pid": p.HostPID, "host_dir": p.HostDir, "residents": map[string]any{"records": []any{release.Resident}, "error": nil}}); err != nil {
					t.Fatal(err)
				}
				release.Resources, err = ReadSessionReadbackFile(resourcesPath, nil)
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(release)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "refused-release":
				if err := maintenanceWrite(filepath.Join(dir, "resident-release-response.json"), map[string]any{"status": "refused", "error": "original owner refused"}); err != nil {
					t.Fatal(err)
				}
			case "running-waiter", "missing-exit":
				wait := map[string]any{"output": "not a terminal original waiter"}
				if kind == "running-waiter" {
					wait["exit_code"], wait["session_id"] = 0, p.LauncherSession
				}
				name := filepath.Join(t.TempDir(), "wait.json")
				if err := maintenanceWrite(name, wait); err != nil {
					t.Fatal(err)
				}
				var err error
				exit.WaitResult, err = ReadSessionReadbackFile(name, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(exit)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(exitPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, _, err = ValidateProgressMaintenanceClosure(ref, p)
			if (err != nil) != (kind != "valid") {
				t.Fatalf("maintenance closure %s: %v", kind, err)
			}
		})
	}
}

func TestProgressMaintenanceRefusesChangedArchivesAndUnknownExecution(t *testing.T) {
	for _, kind := range []string{"archive", "exec", "progress-stop", "prior-stop", "context"} {
		t.Run(kind, func(t *testing.T) {
			r, f, p, _ := maintenanceFixture(t)
			if kind == "archive" {
				if err := os.WriteFile(p.Archives["noodle-result.tar"].Path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if kind == "context" {
				p.ContextID = "foreign"
			} else {
				var c OpenShellCustodyState
				if _, err := ReadSessionReadbackFile(p.Sources["custody"].Path, &c); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "exec":
					c.Records[0].Exec.Result.ExitObserved = false
				case "progress-stop":
					c.Records[0].ProgressStop.Result.StreamError = "unresolved stream"
				case "prior-stop":
					c.Records[0].StopRequestID = "prior"
				}
				data, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.Sources["custody"].Path, data, 0600); err != nil {
					t.Fatal(err)
				}
				p.Sources["custody"], err = ReadSessionReadbackFile(p.Sources["custody"].Path, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateProgressMaintenance(p, r.config.Profile); err == nil {
				t.Fatal("unsafe maintenance inputs accepted")
			}
			data, err := os.ReadFile(filepath.Join(f.root, "events"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "stop\n") {
				t.Fatal("invalid maintenance performed native stop")
			}
			// Cleanup uses the original in-memory fixture owner, not edited test bytes.
			c := f.owner.custody
			record := c.Snapshot().Records[0]
			if err := c.StopSandbox(t.Context(), record.Worker.Identity.Sandbox); err != nil {
				t.Fatal(err)
			}
			if _, err := f.owner.Release("a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
