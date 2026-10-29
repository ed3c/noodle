package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/admission"
)

// A last-lease release is distinct from an unsolicited child exit. A missing
// response remains inspectable only through the original owner's stop intent.
func ProgressMaintenanceReleaseSources(ref SessionReadbackFileRef, p ProgressMaintenance) (SessionReadbackFileRef, *SessionReadbackFileRef, time.Time, error) {
	var intent struct {
		Plan    SessionReadbackFileRef `json:"plan"`
		At      time.Time              `json:"requested_at"`
		Request struct {
			Operation  string `json:"operation"`
			InstanceID string `json:"instance_id"`
			ContextID  string `json:"context_id"`
			RequestID  string `json:"request_id"`
			ResidentID string `json:"resident_id"`
		} `json:"request"`
	}
	var request struct {
		RequestID string `json:"request_id"`
	}
	fail := func(err error) (SessionReadbackFileRef, *SessionReadbackFileRef, time.Time, error) {
		return SessionReadbackFileRef{}, nil, time.Time{}, err
	}
	if err := maintenanceVerify(p.Request, &request); err != nil {
		return fail(err)
	}
	intentRef, err := ReadSessionReadbackFile(filepath.Join(p.SessionDirectory, "maintenance-release-intent.json"), &intent)
	if err != nil {
		return fail(err)
	}
	if intent.Plan != ref || intent.At.IsZero() || p.Resident.StopRequested != "" || intent.Request.Operation != "resident_release_bound" || intent.Request.InstanceID != p.HostInstance || intent.Request.ContextID != p.ContextID || intent.Request.RequestID != request.RequestID || intent.Request.ResidentID != p.Resident.ID {
		return fail(fmt.Errorf("maintenance original resident release intent differs"))
	}
	var response struct {
		Status     string             `json:"status"`
		Error      json.RawMessage    `json:"error"`
		InstanceID string             `json:"host_instance_id"`
		ContextID  string             `json:"context_id"`
		Resident   admission.Resident `json:"resident"`
	}
	responseRef, err := ReadSessionReadbackFile(filepath.Join(filepath.Dir(ref.Path), "resident-release-response.json"), &response)
	if os.IsNotExist(err) {
		return intentRef, nil, intent.At, nil
	}
	if err != nil {
		return fail(err)
	}
	if (response.Status != "stopping" && response.Status != "exited") || (len(response.Error) != 0 && string(response.Error) != "null" && string(response.Error) != `""`) || response.InstanceID != p.HostInstance || response.ContextID != p.ContextID || !ProgressMaintenanceResidentMatches(response.Resident, p.Resident) || response.Resident.StopRequested == "" {
		return fail(fmt.Errorf("original Host refused or did not acknowledge the bound resident release"))
	}
	return intentRef, &responseRef, intent.At, nil
}
