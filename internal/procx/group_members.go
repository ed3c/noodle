package procx

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GroupHasOtherMembers is read-only; the caller must still own the unreaped leader.
func GroupHasOtherMembers(leader int) (bool, error) {
	if leader <= 0 {
		return false, fmt.Errorf("invalid original group leader %d", leader)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=").Output()
	if err != nil {
		return false, fmt.Errorf("observe original group members: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return false, fmt.Errorf("invalid process group observation")
		}
		pid, e1 := strconv.Atoi(fields[0])
		group, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil || pid <= 0 || group < 0 {
			return false, fmt.Errorf("invalid process group identity")
		}
		if group == leader && pid != leader {
			return true, nil
		}
	}
	return false, nil
}
