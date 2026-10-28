package admission

import (
	"slices"
	"time"
)

// These are sampled ancestry observations, not authority to signal a PID.
type ResidentDescendant struct {
	PID          int    `json:"pid"`
	Started      string `json:"started"`
	ParentPID    int    `json:"parent_pid"`
	ProcessGroup int    `json:"process_group"`
	ObservedAt   string `json:"observed_at"`
}

func rememberResidentDescendants(v *Resident, rows []ProcessObservation) {
	if !slices.ContainsFunc(rows, func(p ProcessObservation) bool {
		return p.PID == v.PID && p.Started == v.Started && v.Started != ""
	}) {
		return
	}
	for _, p := range rows {
		if !slices.Contains(p.AncestorPIDs, v.PID) || slices.ContainsFunc(v.ObservedDescendants, func(d ResidentDescendant) bool {
			return d.PID == p.PID && d.Started == p.Started
		}) {
			continue
		}
		v.ObservedDescendants = append(v.ObservedDescendants, ResidentDescendant{
			PID: p.PID, Started: p.Started, ParentPID: p.ParentPID, ProcessGroup: p.ProcessGroup,
			ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
}

func residentDescendantsPresent(v *Resident, rows []ProcessObservation) bool {
	return slices.ContainsFunc(v.ObservedDescendants, func(d ResidentDescendant) bool {
		return slices.ContainsFunc(rows, func(p ProcessObservation) bool { return p.PID == d.PID && p.Started == d.Started })
	})
}
