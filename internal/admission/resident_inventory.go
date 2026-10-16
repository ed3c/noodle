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
	PID                 int    `json:"pid"`
	ParentPID           int    `json:"parent_pid"`
	Started             string `json:"started"`
	RSSKiB              int64  `json:"rss_kib"`
	Executable          string `json:"executable"`
	Ownership           string `json:"ownership"`
	DuplicateCandidates int    `json:"same_parent_executable_count"`
}

func ObserveResidents(ctx context.Context) ([]ProcessObservation, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("resident process inventory unsupported on %s", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-U", strconv.Itoa(os.Getuid()), "-o", "pid=,ppid=,lstart=,rss=,comm=")
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
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 9 {
			return nil, fmt.Errorf("resident process row incomplete")
		}
		pid, e1 := strconv.Atoi(f[0])
		parent, e2 := strconv.Atoi(f[1])
		rss, e3 := strconv.ParseInt(f[7], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || pid <= 0 || rss < 0 {
			return nil, fmt.Errorf("resident process identity or RSS invalid")
		}
		p := ProcessObservation{PID: pid, ParentPID: parent, Started: strings.Join(f[2:7], " "), RSSKiB: rss, Executable: strings.Join(f[8:], " "), Ownership: "needs_owner_readback"}
		out = append(out, p)
		counts[strconv.Itoa(parent)+"/"+p.Executable]++
	}
	for i := range out {
		out[i].DuplicateCandidates = counts[strconv.Itoa(out[i].ParentPID)+"/"+out[i].Executable]
	}
	return out, nil
}
