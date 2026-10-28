package admission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const residentStopGrace = 2 * time.Second

func (r *Residents) forceAfterGrace(v *Resident, child *residentChild) {
	timer := time.NewTimer(residentStopGrace)
	defer timer.Stop()
	select {
	case <-child.done:
		return
	case <-timer.C:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records[v.ID] != v || r.children[v.ID] != child || v.State != "stopping" || len(v.Contexts) != 0 || !v.ForceAfterGrace || r.failure != nil {
		return
	}
	select {
	case <-child.done:
		return
	default:
	}
	v.ForceRequested = time.Now().UTC().Format(time.RFC3339Nano)
	if err := r.persist(); err != nil {
		return
	}
	if err := signalResident(v, child, true); err != nil && !errors.Is(err, os.ErrProcessDone) {
		v.State = "needs_owner_readback"
		v.Error = fmt.Sprintf("force stop outcome requires readback: %v", err)
		_ = r.persist()
	}
	// Only the original Wait plus group-absence readback releases the allocation.
}

func signalResident(v *Resident, child *residentChild, force bool) error {
	child.mu.Lock()
	defer child.mu.Unlock()
	if child.reaped {
		return os.ErrProcessDone
	}
	rows, err := ObserveResidents(context.Background())
	if err != nil {
		return err
	}
	if err := residentStopIdentity(v, child, rows); err != nil {
		return err
	}
	if v.TestFixture {
		return signalFixtureChild(child.process, force)
	}
	if force {
		return forceResident(child.process)
	}
	return stopResident(child.process)
}

func residentStopIdentity(v *Resident, child *residentChild, rows []ProcessObservation) error {
	if child.process == nil || child.process.Pid != v.PID || v.PID <= 0 || v.ProcessGroup != v.PID || v.Started == "" {
		return errors.New("original child identity unavailable")
	}
	for _, row := range rows {
		if row.PID == v.PID && row.Started == v.Started && row.ParentPID == os.Getpid() {
			return nil
		}
	}
	return errors.New("original child PID/start/parent no longer matches")
}
