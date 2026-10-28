package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func restoreProgressMaintenance(ctx context.Context, ref loopruntime.SessionReadbackFileRef, p loopruntime.ProgressMaintenance, profile loopruntime.OpenShellProfile) (loopruntime.SessionReadbackFileRef, error) {
	fail := func(err error) (loopruntime.SessionReadbackFileRef, error) {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	var binding hostRequestBinding
	requestRef, err := loopruntime.ReadSessionReadbackFile(p.Request.Path, &binding)
	if err != nil {
		return fail(err)
	}
	if requestRef != p.Request || binding.HostDir != p.HostDir || binding.RequiredHostInstanceID != p.HostInstance || filepath.Join(binding.ProjectDir, ".noodle", "sessions", filepath.Base(p.SessionDirectory)) != p.SessionDirectory {
		return fail(fmt.Errorf("maintenance offline request differs"))
	}
	hostLock, err := lockfile.TryLock(filepath.Join(p.HostDir, "host.lock"))
	if err != nil {
		return fail(fmt.Errorf("original Host is still owned: %w", err))
	}
	defer hostLock.Close()
	projectLock, err := lockfile.TryLock(filepath.Join(binding.ProjectDir, ".noodle", "noodle.lock"))
	if err != nil {
		return fail(err)
	}
	defer projectLock.Close()
	if procx.IsPIDAlive(p.HostPID) {
		return fail(fmt.Errorf("original Host process absence is unresolved"))
	}
	releaseRef, exitRef, err := loopruntime.ValidateProgressMaintenanceClosure(ref, p)
	if err != nil {
		return fail(err)
	}
	var rows map[string]admission.Resident
	if _, err = loopruntime.ReadSessionReadbackFile(filepath.Join(p.HostDir, "residents.json"), &rows); err != nil {
		return fail(err)
	}
	matched := 0
	for _, row := range rows {
		if row.State != "exited" || len(row.Contexts) != 0 {
			return fail(fmt.Errorf("original resident release is unresolved"))
		}
		if sameMaintenanceResident(row, p.Resident) && slices.Contains(row.Released, p.ContextID) {
			matched++
		}
	}
	if matched != 1 {
		return fail(fmt.Errorf("original resident generation release is missing"))
	}
	absent, err := procx.ProcessGroupAbsent(p.Resident.ProcessGroup)
	if err != nil {
		return fail(err)
	}
	if !absent {
		return fail(fmt.Errorf("original gateway process group remains"))
	}
	u, err := url.Parse(profile.Gateway.Binding.Gateway)
	if err != nil {
		return fail(err)
	}
	conn, dialErr := net.DialTimeout("tcp", u.Host, time.Second)
	if dialErr == nil {
		_ = conn.Close()
		return fail(fmt.Errorf("original gateway endpoint is still open"))
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return fail(fmt.Errorf("original gateway closure is unknown: %w", dialErr))
	}
	directory := filepath.Dir(ref.Path)
	receiptsPath := filepath.Join(p.HostDir, "receipts.json")
	var receipts []hostReceipt
	_, err = loopruntime.ReadSessionReadbackFile(receiptsPath, &receipts)
	if err != nil {
		return fail(err)
	}
	index := -1
	for i, r := range receipts {
		if r.RequestID == binding.RequestID {
			if index >= 0 {
				return fail(fmt.Errorf("maintenance original Host receipt is ambiguous"))
			}
			index = i
		}
	}
	if index < 0 {
		return fail(fmt.Errorf("maintenance original Host receipt is missing"))
	}
	original := receipts[index]
	if original.HostInstanceID != p.HostInstance || original.HostPID != p.HostPID || original.ContextID != p.ContextID || original.ConfigSHA256 != binding.ConfigSHA256 || original.ConfigPath != binding.ConfigPath || original.BinarySHA256 != binding.BinarySHA256 || original.ProjectDir != binding.ProjectDir || original.HostDir != binding.HostDir || original.Mode != binding.Mode || original.CleanupCompleted != binding.CleanupCompleted || (original.ContextState != "failed" && original.ContextState != "active") {
		return fail(fmt.Errorf("maintenance original failed Host receipt differs"))
	}
	before, err := os.ReadFile(receiptsPath)
	if err != nil {
		return fail(err)
	}
	beforePath := filepath.Join(directory, "host-receipts-before.json")
	if err = writeMaintenanceExclusive(beforePath, before); err != nil {
		return fail(err)
	}
	var handoffRef loopruntime.SessionReadbackFileRef
	_, err = loopruntime.RestoreProgressMaintenance(ctx, ref, profile, func(runtimeRef loopruntime.SessionReadbackFileRef) error {
		receipts[index].ContextState = "detached"
		receipts[index].TerminalCause = "maintenance-handoff"
		after, err := json.MarshalIndent(receipts, "", "  ")
		if err != nil {
			return err
		}
		afterPath := filepath.Join(directory, "host-receipts-after.json")
		if err := writeMaintenanceExclusive(afterPath, after); err != nil {
			return err
		}
		if err := filex.WriteFileAtomicDurable(receiptsPath, after); err != nil {
			return err
		}
		beforeRef, err := loopruntime.ReadSessionReadbackFile(beforePath, nil)
		if err != nil {
			return err
		}
		afterRef, err := loopruntime.ReadSessionReadbackFile(afterPath, nil)
		if err != nil {
			return err
		}
		proof := loopruntime.ProgressMaintenanceHostHandoff{Schema: 1, Completion: runtimeRef, HostInstance: p.HostInstance, HostReceiptsBefore: beforeRef, HostReceiptsAfter: afterRef, Release: releaseRef, HostExit: exitRef}
		data, err := json.MarshalIndent(proof, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(directory, "host-handoff.json")
		if err := writeMaintenanceExclusive(path, data); err != nil {
			return err
		}
		handoffRef, err = loopruntime.ReadSessionReadbackFile(path, nil)
		return err
	})
	if err != nil {
		return fail(err)
	}
	return handoffRef, nil
}

func writeMaintenanceExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr, closeErr := f.Sync(), f.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return filex.SyncDir(filepath.Dir(path))
}
