package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/guestprogress"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ProgressMaintenance is an explicit handover of one drained, archived attempt.
// References to private original records never expose their command environment.
type ProgressMaintenance struct {
	Schema                           int
	HostDir, HostInstance, ContextID string
	HostPID                          int
	Request, Profile                 SessionReadbackFileRef
	SessionDirectory                 string
	Sources                          map[string]SessionReadbackFileRef
	Originals                        map[string]SessionReadbackFileRef
	Archives                         map[string]SessionReadbackFileRef
	Resident                         admission.Resident
	Launcher                         SessionReadbackFileRef
	LauncherCall                     SessionReadbackFileRef
	LauncherSession                  int
}

type ProgressMaintenanceStop struct {
	Plan                     SessionReadbackFileRef
	RequestID                string
	Acknowledgement, Stopped SessionReadbackFileRef
}

type ProgressMaintenanceCompletion struct {
	Schema                                        int
	Plan, Stop, OriginalDispatch, OriginalCustody SessionReadbackFileRef
	ContextID, SessionID, OrderID, ProgressHead   string
	RestoreRepair                                 *SessionReadbackFileRef
}

type ProgressMaintenanceRestoreRepair struct {
	Schema                                         int
	Plan, RestoreIntent, CustodyBefore, HostBefore SessionReadbackFileRef
	HostHistory                                    SessionReadbackFileRef
}

type ProgressMaintenanceHostHistory struct {
	Schema   int
	Receipts []SessionReadbackFileRef
}

func maintenanceWrite(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return maintenanceWriteBytes(name, data)
}

func maintenanceWriteBytes(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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
	return filex.SyncDir(filepath.Dir(name))
}

func maintenanceWriteNative(name string, value proto.Message) error {
	data, err := protojson.Marshal(value)
	if err != nil {
		return err
	}
	return maintenanceWriteBytes(name, append(data, '\n'))
}

func maintenanceReadNative(ref SessionReadbackFileRef, value proto.Message) error {
	if err := maintenanceVerify(ref, nil); err != nil {
		return err
	}
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(data, value)
}

func maintenanceVerify(ref SessionReadbackFileRef, value any) error {
	actual, err := ReadSessionReadbackFile(ref.Path, value)
	if err != nil {
		return err
	}
	if actual != ref {
		return fmt.Errorf("maintenance source changed: %s", ref.Path)
	}
	return nil
}

func maintenanceInputs(p ProgressMaintenance, profile OpenShellProfile) (OpenShellProfile, openShellDispatchReceipt, OpenShellCustodyState, OpenShellCustodyRecord, guestprogress.Receipt, error) {
	var d openShellDispatchReceipt
	var c OpenShellCustodyState
	var record OpenShellCustodyRecord
	var saved guestprogress.Receipt
	fail := func(err error) (OpenShellProfile, openShellDispatchReceipt, OpenShellCustodyState, OpenShellCustodyRecord, guestprogress.Receipt, error) {
		return OpenShellProfile{}, d, c, record, saved, err
	}
	if p.Schema != 1 || p.HostPID <= 1 || p.HostInstance == "" || p.ContextID == "" || !filepath.IsAbs(p.HostDir) || !filepath.IsAbs(p.SessionDirectory) {
		return fail(fmt.Errorf("maintenance identity is incomplete"))
	}
	if err := ValidateProgressMaintenanceLauncher(p); err != nil {
		return fail(err)
	}
	if err := maintenanceVerify(p.Profile, nil); err != nil {
		return fail(err)
	}
	if profile.SourcePath != p.Profile.Path || profile.sourceFiles[p.Profile.Path] != p.Profile.SHA256 {
		return fail(fmt.Errorf("maintenance loaded profile differs"))
	}
	if err := VerifyOpenShellProfile(profile); err != nil {
		return fail(err)
	}
	if err := maintenanceVerify(p.Sources["dispatch"], &d); err != nil {
		return fail(err)
	}
	if err := maintenanceVerify(p.Sources["custody"], &c); err != nil {
		return fail(err)
	}
	if err := VerifySessionReadbackFiles(p.Sources); err != nil {
		return fail(err)
	}
	if err := maintenanceVerify(p.Request, nil); err != nil {
		return fail(err)
	}
	profileSHA, err := OpenShellDispatchSHA256(profile)
	if err != nil {
		return fail(err)
	}
	if d.Version != 1 || d.ProfileSHA256 != profileSHA || d.ContextID != p.ContextID || filepath.Base(p.SessionDirectory) != d.SessionID || d.Phase != "progress-unknown" || d.Error == "" || d.Baseline == nil || d.Worker == nil || d.Progress != nil || d.ProgressHead != "" || d.CandidateHead != "" || d.Delivery != nil {
		return fail(fmt.Errorf("maintenance requires the original unreturned progress failure"))
	}
	if c.Version != openShellCustodyVersion || c.Binding != profile.Gateway.Binding || c.Blocked != "" {
		return fail(fmt.Errorf("maintenance custody differs"))
	}
	count := 0
	for _, r := range c.Records {
		if r.Worker.Identity == d.Worker.Identity {
			record = r
			count++
		} else if !r.Retained && r.Stopped == nil {
			return fail(fmt.Errorf("another original worker is unsettled"))
		}
	}
	if count != 1 || record.Task != (OpenShellTask{d.OrderID, d.AttemptID, d.SessionID}) || !record.CreateAcknowledged || record.Retained || record.StopRequestID != "" || record.StopAcknowledged || record.Stopped != nil || record.Exec == nil || record.ProgressStop == nil {
		return fail(fmt.Errorf("maintenance worker has unresolved or prior stop effects"))
	}
	for _, e := range []*OpenShellCustodyExec{record.Exec, record.ProgressStop} {
		if e.Result.RequestID != e.Command.RequestID || !e.Result.StartAttempted || !e.Result.ExitObserved || e.Result.ExitCode != 0 || e.Result.InputError != "" || e.Result.StreamError != "" {
			return fail(fmt.Errorf("maintenance native execution is unresolved"))
		}
	}
	_, binding, err := unwrapOpenShellProgress(record.Exec.Command)
	if err != nil {
		return fail(err)
	}
	control, err := progressStopCommand(record.Exec.Command, record.ProgressStop.Command.RequestID)
	if err != nil || !reflect.DeepEqual(control, record.ProgressStop.Command) {
		return fail(fmt.Errorf("maintenance progress-stop command differs"))
	}
	if err := maintenanceVerify(p.Archives["result.json"], &saved); err != nil {
		return fail(err)
	}
	var originalSaved guestprogress.Receipt
	if err := maintenanceVerify(p.Sources["stop-output"], &originalSaved); err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(originalSaved, saved) {
		return fail(fmt.Errorf("maintenance archives differ from the original stop output"))
	}
	token := sha256.Sum256([]byte(binding.Token))
	id := guestprogress.Identity{SessionID: d.SessionID, CommandSHA256: binding.CommandSHA256, TokenSHA256: hex.EncodeToString(token[:])}
	if err := guestprogress.ValidateReceipt(saved, id, record.ProgressStop.Command.RequestID); err != nil {
		return fail(err)
	}
	if err := VerifySessionReadbackFiles(p.Archives); err != nil {
		return fail(err)
	}
	if len(p.Archives) != 3 {
		return fail(fmt.Errorf("maintenance archive set differs"))
	}
	for _, archive := range saved.Archives {
		ref := p.Archives[archive.Name]
		info, err := os.Stat(ref.Path)
		if err != nil || ref.SHA256 != archive.SHA256 || info.Size() != archive.Bytes {
			return fail(fmt.Errorf("maintenance archive differs: %s", archive.Name))
		}
	}
	return profile, d, c, record, saved, nil
}

type ProgressMaintenanceHostHandoff struct {
	Schema                                int
	Completion                            SessionReadbackFileRef
	HostInstance                          string
	HostReceiptsBefore, HostReceiptsAfter SessionReadbackFileRef
	Release, HostExit                     SessionReadbackFileRef
}

type ProgressMaintenanceRelease struct {
	Plan, Resources SessionReadbackFileRef
	Intent          SessionReadbackFileRef
	Response        *SessionReadbackFileRef
	Resident        admission.Resident
	ObservedAt      time.Time
}

type ProgressMaintenanceExit struct {
	Plan, WaitResult         SessionReadbackFileRef
	WaitCall                 SessionReadbackFileRef
	HostInstance             string
	HostPID, LauncherSession int
	ObservedAt               time.Time
}

func ValidateProgressMaintenance(p ProgressMaintenance, profile OpenShellProfile) error {
	_, _, _, _, _, err := maintenanceInputs(p, profile)
	return err
}

func WriteProgressMaintenance(name string, p ProgressMaintenance, profile OpenShellProfile) (SessionReadbackFileRef, error) {
	if err := ValidateProgressMaintenance(p, profile); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if len(p.Originals) != len(p.Sources) {
		return SessionReadbackFileRef{}, fmt.Errorf("maintenance original snapshots are incomplete")
	}
	for key, ref := range p.Sources {
		if p.Originals[key].SHA256 != ref.SHA256 {
			return SessionReadbackFileRef{}, fmt.Errorf("maintenance snapshot differs: %s", key)
		}
	}
	if err := VerifySessionReadbackFiles(p.Originals); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceWrite(name, p); err != nil {
		return SessionReadbackFileRef{}, err
	}
	return ReadSessionReadbackFile(name, nil)
}
