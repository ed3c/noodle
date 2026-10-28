//go:build darwin && cgo

package procx

/*
#cgo LDFLAGS: -lbsm
#include <bsm/libbsm.h>
#include <dlfcn.h>
#include <errno.h>
#include <libproc.h>
#include <mach/mach.h>
#include <signal.h>
#include <sys/proc.h>
#include <string.h>

static int fixture_identity(int pid, struct proc_bsdinfo *info, audit_token_t *token) {
    if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, info, sizeof(*info)) != sizeof(*info)) {
        return errno ? errno : EIO;
    }
    if (info->pbi_status == SZOMB) return 0;
    mach_port_t task = MACH_PORT_NULL;
    kern_return_t kr = task_name_for_pid(mach_task_self(), pid, &task);
    if (kr != KERN_SUCCESS) return EACCES;
    mach_msg_type_number_t count = TASK_AUDIT_TOKEN_COUNT;
    kr = task_info(task, TASK_AUDIT_TOKEN, (task_info_t)token, &count);
    mach_port_deallocate(mach_task_self(), task);
    if (kr != KERN_SUCCESS || count != TASK_AUDIT_TOKEN_COUNT) return EACCES;
    if (audit_token_to_pid(*token) != pid || audit_token_to_euid(*token) != info->pbi_uid) return ESTALE;
    return 0;
}
static int fixture_signal(audit_token_t *token, int force) {
    typedef int (*signal_fn)(audit_token_t *, int);
    signal_fn fn = (signal_fn)dlsym(RTLD_DEFAULT, "proc_signal_with_audittoken");
    if (!fn) return ENOTSUP;
    return fn(token, force ? SIGKILL : SIGTERM);
}
static int fixture_control_available(void) {return dlsym(RTLD_DEFAULT, "proc_signal_with_audittoken") != NULL;}
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

func NativeFixtureControlAvailable() bool { return C.fixture_control_available() != 0 }

func ObserveNativeIdentity(pid int) (NativeIdentity, error) {
	if pid <= 1 {
		return NativeIdentity{}, fmt.Errorf("invalid fixture process identity")
	}
	var info C.struct_proc_bsdinfo
	var token C.audit_token_t
	code := C.fixture_identity(C.int(pid), &info, &token)
	if code != 0 {
		if code == C.ESRCH {
			return NativeIdentity{}, ErrProcessGone
		}
		return NativeIdentity{}, fmt.Errorf("kernel process identity: %w", syscall.Errno(code))
	}
	p := NativeIdentity{PID: pid, UID: int(info.pbi_uid), ParentPID: int(info.pbi_ppid), Group: int(info.pbi_pgid),
		StartSec: uint64(info.pbi_start_tvsec), StartUsec: uint64(info.pbi_start_tvusec), Zombie: info.pbi_status == C.SZOMB}
	if !p.Zombie {
		p.Version = int(C.audit_token_to_pidversion(token))
		p.Token = C.GoBytes(unsafe.Pointer(&token), C.int(C.sizeof_audit_token_t))
	}
	return p, nil
}

// SignalNativeIdentity targets one kernel generation, never a process group.
// The kernel validates PID version again when obtaining the target proc ref.
func SignalNativeIdentity(p NativeIdentity, force bool) error {
	if p.PID <= 1 || p.Version == 0 || len(p.Token) != int(C.sizeof_audit_token_t) {
		return fmt.Errorf("kernel audit token unavailable")
	}
	var token C.audit_token_t
	C.memcpy(unsafe.Pointer(&token), unsafe.Pointer(&p.Token[0]), C.size_t(len(p.Token)))
	if int(C.audit_token_to_pid(token)) != p.PID || int(C.audit_token_to_pidversion(token)) != p.Version {
		return fmt.Errorf("kernel audit token does not match process identity")
	}
	var hard C.int
	if force {
		hard = 1
	}
	if code := C.fixture_signal(&token, hard); code != 0 {
		return fmt.Errorf("audit-token signal: %w", syscall.Errno(code))
	}
	return nil
}
