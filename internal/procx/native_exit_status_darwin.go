//go:build darwin && cgo

package procx

/*
#include <bsm/libbsm.h>
#include <errno.h>
#include <libproc.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>
#include <sys/proc.h>

// Private read-only ABI: Apple xnu/bsd/sys/proc_info_private.h,
// PROC_PIDT_BSDINFOWITHUNIQID. Keep its layout explicit; do not fall back to a
// PID/start-only read when unavailable. No Apple implementation is vendored.
struct noodle_proc_unique {
    uint8_t image_uuid[16];
    uint64_t unique_id, parent_unique_id;
    int32_t id_version, original_parent_version;
    uint64_t reserved[2];
};
struct noodle_bsd_unique {
    struct proc_bsdinfo bsd;
    struct noodle_proc_unique unique;
};
_Static_assert(sizeof(struct proc_bsdinfo) == 136, "unsupported BSD process ABI");
_Static_assert(sizeof(struct noodle_proc_unique) == 56, "unsupported unique process ABI");
_Static_assert(offsetof(struct noodle_proc_unique, id_version) == 32, "unsupported process version offset");
_Static_assert(offsetof(struct noodle_bsd_unique, unique) == 136, "unsupported combined process offset");
_Static_assert(sizeof(struct noodle_bsd_unique) == 192, "unsupported combined process ABI");
_Static_assert(SZOMB == 5, "unsupported zombie state ABI");

static int noodle_native_exit(audit_token_t *token, int pid, int uid, int version,
                              struct noodle_bsd_unique *info) {
    if (audit_token_to_pid(*token) != pid || audit_token_to_euid(*token) != uid ||
        audit_token_to_pidversion(*token) != version) return EINVAL;
    errno = 0;
    // arg=1 includes zombies. Both status and pidversion come from one proc ref.
    int copied = proc_pidinfo(pid, 18, 1, info, sizeof(*info));
    if (copied != sizeof(*info)) return copied > 0 ? EIO : (errno ? errno : EIO);
    return 0;
}
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ObserveNativeExitStatus reads an unreaped original generation without waiting,
// signalling, reaping or resuming its parent. Missing evidence remains unknown.
func ObserveNativeExitStatus(original NativeIdentity) (NativeExitStatus, error) {
	var result NativeExitStatus
	if original.PID <= 1 || !original.SameGeneration(original) || len(original.Token) != int(C.sizeof_audit_token_t) {
		return result, fmt.Errorf("original kernel exit identity is incomplete")
	}
	var token C.audit_token_t
	C.memcpy(unsafe.Pointer(&token), unsafe.Pointer(&original.Token[0]), C.size_t(len(original.Token)))
	var info C.struct_noodle_bsd_unique
	if code := C.noodle_native_exit(&token, C.int(original.PID), C.int(original.UID), C.int(original.Version), &info); code != 0 {
		return result, fmt.Errorf("kernel exit status unavailable: %w", syscall.Errno(code))
	}
	bsd := info.bsd
	result = NativeExitStatus{PID: int(bsd.pbi_pid), UID: int(bsd.pbi_uid), ParentPID: int(bsd.pbi_ppid), Group: int(bsd.pbi_pgid),
		StartSec: uint64(bsd.pbi_start_tvsec), StartUsec: uint64(bsd.pbi_start_tvusec), Version: int(info.unique.id_version),
		State: uint32(bsd.pbi_status), WaitStatus: uint32(bsd.pbi_xstatus)}
	if result.PID != original.PID || result.UID != original.UID || result.ParentPID != original.ParentPID || result.Group != original.Group || result.StartSec != original.StartSec || result.StartUsec != original.StartUsec || result.Version != original.Version {
		return NativeExitStatus{}, fmt.Errorf("original kernel exit generation, exec version or ancestry differs")
	}
	if bsd.pbi_status != C.SZOMB {
		return NativeExitStatus{}, fmt.Errorf("original kernel process is not an unreaped zombie")
	}
	return result, nil
}
