package procx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// WaitChildExit observes an owned child's exit without reaping its PID.
func WaitChildExit(pid int) error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	changes := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC,
		Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(fd, changes, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("observe original child exit: %w", err)
		}
		if n == 1 && events[0].Flags&unix.EV_ERROR == 0 && events[0].Fflags&unix.NOTE_EXIT != 0 {
			return nil
		}
		if n == 1 && events[0].Flags&unix.EV_ERROR != 0 && events[0].Data == int64(unix.ESRCH) {
			// Darwin rejects a late kqueue registration on a zombie. The
			// caller has never waited this child, so its PID is still reserved.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			raw, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
			if err == nil && strings.HasPrefix(strings.TrimSpace(string(raw)), "Z") {
				return nil
			}
			return fmt.Errorf("original child zombie readback unconfirmed: %q, %v", raw, err)
		}
		return fmt.Errorf("original child exit notification unavailable: %+v", events[:n])
	}
}
