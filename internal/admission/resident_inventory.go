package admission

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type ProcessObservation struct {
	UID                 *int   `json:"uid,omitempty"`
	PID                 int    `json:"pid"`
	ParentPID           int    `json:"parent_pid"`
	ProcessGroup        int    `json:"process_group"`
	AncestorPIDs        []int  `json:"ancestor_pids"`
	AncestryComplete    bool   `json:"ancestry_complete"`
	Started             string `json:"started"`
	RSSKiB              int64  `json:"rss_kib"`
	Executable          string `json:"executable"`
	Ownership           string `json:"ownership"`
	DuplicateCandidates int    `json:"same_parent_executable_count"`
}

// ObserveVisibleProcesses is a metadata snapshot, never admission or control
// authority. Current-user observations used by child guards remain separate.
func ObserveVisibleProcesses(ctx context.Context) ([]ProcessObservation, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("visible process inventory unsupported on %s", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,uid=,lstart=,rss=,comm=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("visible process inventory: %w", err)
	}
	return parseVisibleProcesses(string(data))
}

func parseVisibleProcesses(data string) ([]ProcessObservation, error) {
	uids := map[int]int{}
	lines := []string{}
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 11 {
			return nil, fmt.Errorf("visible process identity fields incomplete")
		}
		pid, e1 := strconv.Atoi(f[0])
		uid, e2 := strconv.Atoi(f[3])
		// macOS ps prints some uid_t system identities in signed form (for
		// example nobody is -2). Preserve that observation without authorizing it.
		if e1 != nil || e2 != nil || pid <= 0 || uid < -1<<31 || uid > 1<<32-1 {
			return nil, fmt.Errorf("visible process PID/UID invalid")
		}
		if _, exists := uids[pid]; exists {
			return nil, fmt.Errorf("visible process PID duplicated")
		}
		uids[pid] = uid
		lines = append(lines, strings.Join(append(f[:3], f[4:]...), " "))
	}
	rows, err := parseResidents(strings.Join(lines, "\n"))
	if err != nil {
		return nil, err
	}
	for i := range rows {
		uid := uids[rows[i].PID]
		rows[i].UID = &uid
	}
	return rows, nil
}

func ObserveResidents(ctx context.Context) ([]ProcessObservation, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("resident process inventory unsupported on %s", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-U", strconv.Itoa(os.Getuid()), "-o", "pid=,ppid=,pgid=,lstart=,rss=,comm=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("resident inventory: %w", err)
	}
	return parseResidents(string(data))
}
func parseResidents(data string) ([]ProcessObservation, error) {
	out := []ProcessObservation{}
	counts := map[string]int{}
	parents := map[int]int{}
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 10 {
			return nil, fmt.Errorf("resident process row incomplete")
		}
		pid, e1 := strconv.Atoi(f[0])
		parent, e2 := strconv.Atoi(f[1])
		group, e3 := strconv.Atoi(f[2])
		rss, e4 := strconv.ParseInt(f[8], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || pid <= 0 || parent < 0 || group < 0 || rss < 0 {
			return nil, fmt.Errorf("resident process identity or RSS invalid")
		}
		if _, exists := parents[pid]; exists {
			return nil, fmt.Errorf("resident process identity duplicated")
		}
		parents[pid] = parent
		p := ProcessObservation{PID: pid, ParentPID: parent, ProcessGroup: group, AncestorPIDs: []int{}, Started: strings.Join(f[3:8], " "), RSSKiB: rss, Executable: strings.Join(f[9:], " "), Ownership: "needs_owner_readback"}
		out = append(out, p)
		counts[strconv.Itoa(parent)+"/"+p.Executable]++
	}
	for i := range out {
		out[i].DuplicateCandidates = counts[strconv.Itoa(out[i].ParentPID)+"/"+out[i].Executable]
		seen := map[int]bool{out[i].PID: true}
		parent := out[i].ParentPID
		for parent != 0 {
			if seen[parent] {
				return nil, fmt.Errorf("resident process ancestry cycle")
			}
			seen[parent] = true
			out[i].AncestorPIDs = append(out[i].AncestorPIDs, parent)
			next, exists := parents[parent]
			if !exists {
				break
			}
			parent = next
		}
		out[i].AncestryComplete = parent == 0
	}
	return out, nil
}
