package dispatcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type nativeProcessIdentity struct {
	PID     int    `json:"pid"`
	Parent  int    `json:"parent"`
	Started string `json:"started"`
}

func nativeProcessSnapshot(ctx context.Context) (map[int]nativeProcessIdentity, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("native process observation unavailable: %w", err)
	}
	rows := make(map[int]nativeProcessIdentity)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 7 {
			return nil, fmt.Errorf("native process observation has invalid identity")
		}
		pid, e1 := strconv.Atoi(fields[0])
		parent, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil || pid <= 0 || parent < 0 {
			return nil, fmt.Errorf("native process identity invalid")
		}
		rows[pid] = nativeProcessIdentity{PID: pid, Parent: parent, Started: strings.Join(fields[2:], " ")}
	}
	return rows, nil
}

func nativeProcessDescendants(rows map[int]nativeProcessIdentity, root nativeProcessIdentity) ([]nativeProcessIdentity, error) {
	current, ok := rows[root.PID]
	if !ok || current.Started != root.Started {
		return nil, fmt.Errorf("original foreground process identity unavailable")
	}
	owned := map[int]bool{root.PID: true}
	var result []nativeProcessIdentity
	for changed := true; changed; {
		changed = false
		for pid, row := range rows {
			if !owned[pid] && owned[row.Parent] {
				owned[pid] = true
				result = append(result, row)
				changed = true
			}
		}
	}
	return result, nil
}

func nativeObservedProcessesExited(rows map[int]nativeProcessIdentity, original []nativeProcessIdentity) error {
	for _, old := range original {
		if current, ok := rows[old.PID]; ok && current.Started == old.Started {
			return fmt.Errorf("original native child still exists: pid=%d started=%s", old.PID, old.Started)
		}
	}
	return nil
}
