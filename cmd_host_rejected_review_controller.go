package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

// Retire only the old Host. Ordinary successor admission still uses the
// machine-wide resource producer after the original disk and lease readback.
func retireRejectedReviewController(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	root := p.OwnerBefore.Current.HostDir
	pool, err := readHostPool(root)
	if err != nil {
		return result, err
	}
	g, err := rejectedReviewControllerScope(pool, p, intent.SHA256)
	if err != nil {
		return result, err
	}
	if pool.Active == g.ID {
		_, identity, err := residentRetirementResources(ctx, root, g, nativeEmptyHostRetirement(), true)
		if err != nil || !p.Host.Process.SameGeneration(identity) {
			return result, errors.New("original controller zero-context residency cannot be verified")
		}
		directory, err := admission.DefaultMachineCapacityDirectory()
		if err != nil {
			return result, err
		}
		remaining, err := admission.ReadMachineHostReservations(directory, root)
		if err != nil || !slices.Equal(remaining, []string{g.Preparation.ID}) {
			return result, errors.New("original controller has other unresolved source Host reservations")
		}
		if err := saveHostStorageBytes(intent.Path+".controller-retire", []byte(intent.SHA256)); err != nil {
			return result, err
		}
		if err := updateHostPool(root, func(pool *hostPool) error {
			current, err := rejectedReviewControllerScope(*pool, p, intent.SHA256)
			if err != nil {
				return err
			}
			if pool.Active != g.ID || current.State != "active" {
				return errors.New("original active source changed before normal retirement")
			}
			current.State = "retiring"
			pool.Generations[current.ID] = current
			pool.Active = ""
			return nil
		}); err != nil {
			return result, err
		}
		result.Effects = append(result.Effects, "original_controller_normal_retirement_requested")
	}
	wait, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for {
		result, err = readRejectedReviewController(p, intent, result)
		if err != nil || result.Status == "source_host_retired" {
			return result, err
		}
		if result.SourceExit != nil {
			pool, err = readHostPool(root)
			if err != nil {
				return result, err
			}
			g, err = rejectedReviewControllerScope(pool, p, intent.SHA256)
			if err != nil {
				return result, err
			}
			if err := returnHostResidentResources(root, g); err != nil {
				return result, err
			}
			if err := updateHostPool(root, func(pool *hostPool) error {
				current, err := rejectedReviewControllerScope(*pool, p, intent.SHA256)
				if err != nil || pool.Active != "" || current.ResidentExit == nil || *current.ResidentExit != *result.SourceExit {
					return errors.New("original controller exiting witness changed")
				}
				current.State = "exited"
				pool.Generations[current.ID] = current
				return nil
			}); err != nil {
				return result, err
			}
			result.Effects = append(result.Effects, "original_controller_resident_reservation_returned")
			return readRejectedReviewController(p, intent, result)
		}
		select {
		case <-wait.Done():
			return result, nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func rejectedReviewControllerScope(pool hostPool, p rejectedReviewReleasePlan, digest string) (hostGeneration, error) {
	old, ok := pool.Generations[p.Generation.ID]
	frozen := old
	frozen.State, frozen.ResidentExit = p.Generation.State, p.Generation.ResidentExit
	if !ok || !reflect.DeepEqual(frozen, p.Generation) || old.Preparation == nil || len(pool.Owners) != 1 {
		return old, errors.New("original retiring source generation differs")
	}
	owner := p.OwnerBefore
	owner.State, owner.ControllerOnly, owner.MaintenanceHold = "ready", true, rejectedReviewReleaseHold+digest
	owner.Current.ContextState, owner.Current.TerminalCause = "detached", loop.TerminalMigrationDetach
	owner.RoundState = &loop.HostContinuation{Mode: p.LoopMode, DispatchState: loop.StatePaused}
	if !reflect.DeepEqual(pool.Owners[owner.Current.RequestID], owner) {
		return old, errors.New("original paused retained review continuation differs")
	}
	if pool.Active == old.ID && old.State != "active" ||
		pool.Active == "" && old.State != "retiring" && old.State != "exiting" && old.State != "exited" ||
		pool.Active != "" && pool.Active != old.ID {
		return old, errors.New("original controller retirement routing differs")
	}
	for id, generation := range pool.Generations {
		if id != old.ID && generationLive(generation) {
			return old, errors.New("original source root has another live generation")
		}
	}
	for _, pending := range pool.Queue {
		if pending.State == "queued" || pending.State == "starting" || pending.State == "blocked" {
			return old, errors.New("original source root has a competing version request")
		}
	}
	return old, nil
}

func readRejectedReviewController(p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	root := p.OwnerBefore.Current.HostDir
	pool, err := readHostPool(root)
	if err != nil {
		return result, err
	}
	old, err := rejectedReviewControllerScope(pool, p, intent.SHA256)
	if err != nil {
		return result, err
	}
	result.Status, result.Next = "source_controller_pending", p.next("reconcile", intent.SHA256)
	result.SourceExit = nil
	if pool.Active == old.ID {
		result.Next = p.next("finish", intent.SHA256)
		return result, nil
	}
	for _, actor := range []hostOperatorActor{p.Host, p.Gateway} {
		gone, err := procx.ObserveNativeExit(actor.Process)
		if err != nil {
			return result, err
		}
		absent, err := procx.ProcessGroupAbsent(actor.Process.Group)
		if err != nil {
			return result, err
		}
		if !gone || !absent {
			return result, nil
		}
	}
	if old.ResidentExit == nil {
		return result, errors.New("original controller process absent without its own normal shutdown witness")
	}
	exit, err := readResidentExitWitness(root, old)
	if err != nil {
		return result, err
	}
	result.SourceExit = &exit
	result.Next = p.next("finish", intent.SHA256)
	if old.State != "exited" {
		return result, nil
	}
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return result, err
	}
	preparations, err := admission.ReadMachinePreparations(directory, *old.Preparation)
	if err != nil || !preparations[*old.Preparation].Closed {
		return result, errors.New("original resident machine reservation remains held or unknown")
	}
	remaining, err := admission.ReadMachineHostReservations(directory, root)
	if err != nil || len(remaining) != 0 {
		return result, errors.New("original retired Host retains an unresolved machine reservation")
	}
	processes, rss := 0, int64(0)
	result.Status = "source_host_retired"
	result.SourceProcessCount, result.SourceRSS = &processes, &rss
	return result, nil
}
