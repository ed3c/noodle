package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func maintenanceResident(packet map[string]json.RawMessage, p loopruntime.ProgressMaintenance, name string) (admission.Resident, error) {
	var rows struct {
		Records []admission.Resident `json:"records"`
		Error   json.RawMessage      `json:"error"`
	}
	if err := json.Unmarshal(packet["residents"], &rows); err != nil {
		return admission.Resident{}, err
	}
	if string(rows.Error) != "null" {
		return admission.Resident{}, fmt.Errorf("maintenance resident ownership is unresolved")
	}
	var selected admission.Resident
	count := 0
	for _, r := range rows.Records {
		if r.Name == name && r.State == "running" && len(r.Contexts) == 1 && r.Contexts[0] == p.ContextID {
			selected = r
			count++
		} else if r.State != "exited" || len(r.Contexts) != 0 {
			return selected, fmt.Errorf("another resident owns the maintenance context")
		}
	}
	if count != 1 || selected.ID == "" || selected.GuardID == "" || selected.PID <= 1 || selected.ProcessGroup != selected.PID || selected.Started == "" {
		return selected, fmt.Errorf("maintenance original resident generation is unavailable")
	}
	return selected, nil
}

func sameMaintenanceResident(a, b admission.Resident) bool {
	return loopruntime.ProgressMaintenanceResidentMatches(a, b)
}

func releaseProgressMaintenance(ctx context.Context, ref loopruntime.SessionReadbackFileRef, p loopruntime.ProgressMaintenance, profile loopruntime.OpenShellProfile, inspect bool) (loopruntime.SessionReadbackFileRef, error) {
	fail := func(err error) (loopruntime.SessionReadbackFileRef, error) {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	if _, err := loopruntime.ValidateProgressMaintenanceStop(ref, profile); err != nil {
		return fail(err)
	}
	lock, err := lockfile.TryLock(filepath.Join(p.SessionDirectory, "maintenance.lock"))
	if err != nil {
		return fail(err)
	}
	defer lock.Close()
	directory := filepath.Dir(ref.Path)
	if !inspect {
		if err := progressMaintenanceHost(p); err != nil {
			return fail(err)
		}
		var binding hostRequestBinding
		if _, err := loopruntime.ReadSessionReadbackFile(p.Request.Path, &binding); err != nil {
			return fail(err)
		}
		req := hostRequest{Operation: "resident_release_bound", ProjectDir: binding.ProjectDir, ContextID: p.ContextID, InstanceID: p.HostInstance, RequestID: binding.RequestID, ConfigSHA256: binding.ConfigSHA256, ConfigPath: binding.ConfigPath, Mode: binding.Mode, CleanupCompleted: binding.CleanupCompleted, BinarySHA256: binding.BinarySHA256, ResidentID: p.Resident.ID}
		data, err := json.Marshal(map[string]any{"plan": ref, "request": req, "requested_at": time.Now().UTC()})
		if err != nil {
			return fail(err)
		}
		if err := writeMaintenanceExclusive(filepath.Join(p.SessionDirectory, "maintenance-release-intent.json"), data); err != nil {
			return fail(fmt.Errorf("maintenance release already attempted; inspect original owner: %w", err))
		}
		conn, err := net.DialTimeout("unix", filepath.Join(p.HostDir, "host.sock"), time.Second)
		if err != nil {
			return fail(err)
		}
		if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			_ = conn.Close()
			return fail(err)
		}
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			_ = conn.Close()
			return fail(err)
		}
		var response json.RawMessage
		err = json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&response)
		_ = conn.Close()
		if err != nil {
			return fail(fmt.Errorf("original resident release response unresolved; use release-inspect: %w", err))
		}
		if err := writeMaintenanceExclusive(filepath.Join(directory, "resident-release-response.json"), response); err != nil {
			return fail(err)
		}
	} else {
		var intent struct {
			Plan loopruntime.SessionReadbackFileRef `json:"plan"`
		}
		if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(p.SessionDirectory, "maintenance-release-intent.json"), &intent); err != nil {
			return fail(err)
		}
		if intent.Plan != ref {
			return fail(fmt.Errorf("original release intent differs"))
		}
	}
	intentRef, responseRef, requestedAt, err := loopruntime.ProgressMaintenanceReleaseSources(ref, p)
	if err != nil {
		return fail(err)
	}
	for {
		packet, err := progressMaintenanceResources(p.HostDir)
		if err != nil {
			return fail(err)
		}
		var instance string
		var pid int
		if err := json.Unmarshal(packet["host_instance_id"], &instance); err != nil {
			return fail(err)
		}
		if err := json.Unmarshal(packet["host_pid"], &pid); err != nil {
			return fail(err)
		}
		if instance != p.HostInstance || pid != p.HostPID {
			return fail(fmt.Errorf("original Host changed during resident release"))
		}
		var rows struct {
			Records []admission.Resident `json:"records"`
			Error   json.RawMessage      `json:"error"`
		}
		if err := json.Unmarshal(packet["residents"], &rows); err != nil {
			return fail(err)
		}
		if string(rows.Error) != "null" {
			return fail(fmt.Errorf("original resident release is unresolved"))
		}
		for _, r := range rows.Records {
			if sameMaintenanceResident(r, p.Resident) && r.State == "exited" && len(r.Contexts) == 0 && slices.Contains(r.Released, p.ContextID) {
				stopAt, err := time.Parse(time.RFC3339Nano, r.StopRequested)
				if err != nil || stopAt.Before(requestedAt) {
					return fail(fmt.Errorf("original resident exited without this bound release"))
				}
				absent, err := procx.ProcessGroupAbsent(r.ProcessGroup)
				if err != nil {
					return fail(err)
				}
				if !absent {
					return fail(fmt.Errorf("original resident process group remains"))
				}
				bytes, err := json.Marshal(packet)
				if err != nil {
					return fail(err)
				}
				resourcesPath := filepath.Join(directory, "resident-released-resources.json")
				if err := writeMaintenanceExclusive(resourcesPath, bytes); err != nil {
					return fail(err)
				}
				resources, err := loopruntime.ReadSessionReadbackFile(resourcesPath, nil)
				if err != nil {
					return fail(err)
				}
				proof := loopruntime.ProgressMaintenanceRelease{Plan: ref, Resources: resources, Intent: intentRef, Response: responseRef, Resident: r, ObservedAt: time.Now().UTC()}
				data, err := json.Marshal(proof)
				if err != nil {
					return fail(err)
				}
				name := filepath.Join(directory, "release-complete.json")
				if err := writeMaintenanceExclusive(name, data); err != nil {
					return fail(err)
				}
				return loopruntime.ReadSessionReadbackFile(name, nil)
			}
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func maintenanceResidentUnchanged(p loopruntime.ProgressMaintenance, packet map[string]json.RawMessage) error {
	r, err := maintenanceResident(packet, p, p.Resident.Name)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(r, p.Resident) {
		return fmt.Errorf("maintenance original resident changed")
	}
	return nil
}
