package procx

// NativeExitStatus is one kernel snapshot of an unreaped Darwin zombie. It is
// not synthesized from process absence, a requested signal or a live zero field.
type NativeExitStatus struct {
	PID        int    `json:"pid"`
	UID        int    `json:"uid"`
	ParentPID  int    `json:"parent_pid"`
	Group      int    `json:"group"`
	StartSec   uint64 `json:"start_sec"`
	StartUsec  uint64 `json:"start_usec"`
	Version    int    `json:"version"`
	State      uint32 `json:"process_state"`
	WaitStatus uint32 `json:"wait_status"`
}

// Darwin SZOMB=5 and wait status zero together mean normal exit code zero.
// The native accessor checks this ABI and the original process's exec version.
func (s NativeExitStatus) NormalZero() bool { return s.State == 5 && s.WaitStatus == 0 }
