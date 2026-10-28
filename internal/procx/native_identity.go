package procx

import (
	"bytes"
	"errors"
)

var ErrProcessGone = errors.New("original process generation absent")

// NativeIdentity is kernel-observed evidence, not an adopted os.Process handle.
// Token is opaque and must only originate from the platform accessor.
type NativeIdentity struct {
	PID       int    `json:"pid"`
	UID       int    `json:"uid"`
	ParentPID int    `json:"parent_pid"`
	Group     int    `json:"group"`
	StartSec  uint64 `json:"start_sec"`
	StartUsec uint64 `json:"start_usec"`
	Version   int    `json:"version"`
	Zombie    bool   `json:"zombie"`
	Token     []byte `json:"token,omitempty"`
}

func (p NativeIdentity) SameGeneration(q NativeIdentity) bool {
	return p.PID > 0 && p.PID == q.PID && p.UID == q.UID && p.StartSec != 0 &&
		p.StartSec == q.StartSec && p.StartUsec == q.StartUsec && p.Version != 0 &&
		p.Version == q.Version && len(p.Token) != 0 && bytes.Equal(p.Token, q.Token)
}
