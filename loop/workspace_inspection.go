package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/reducer"
)

// InspectionGap reports unavailable facts, never absence of effects.
type InspectionGap struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// InspectionSource names retained files without guessing their association to a context.
type InspectionSource struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
}

// WorkspaceInspection is a persisted observation, not mutation admission or liveness.
type WorkspaceInspection struct {
	ProjectDir     string                   `json:"project_dir"`
	SnapshotPath   string                   `json:"snapshot_path"`
	SnapshotSHA256 string                   `json:"snapshot_sha256"`
	Status         string                   `json:"status"`
	Snapshot       *reducer.DurableSnapshot `json:"snapshot"`
	Sources        []InspectionSource       `json:"sources"`
	Gaps           []InspectionGap          `json:"gaps"`
}

// ReadWorkspaceInspection never creates files, acquires locks or starts a runtime.
func ReadWorkspaceInspection(project string) WorkspaceInspection {
	path := filepath.Join(project, ".noodle", "state.snapshot.json")
	r := WorkspaceInspection{ProjectDir: project, SnapshotPath: path, Status: "unavailable", Gaps: []InspectionGap{}, Sources: []InspectionSource{}}
	readWorkspaceSources(&r)
	data, err := os.ReadFile(path)
	if err != nil {
		code := "unreadable"
		if os.IsNotExist(err) {
			code = "missing"
		}
		r.Gaps = append(r.Gaps, InspectionGap{path, code, err.Error()})
		return r
	}
	sum := sha256.Sum256(data)
	r.SnapshotSHA256 = hex.EncodeToString(sum[:])
	snapshot, err := reducer.DecodeSnapshot(data)
	if err != nil {
		r.Gaps = append(r.Gaps, InspectionGap{path, "invalid", err.Error()})
		return r
	}
	if snapshot.State.SchemaVersion != 1 || snapshot.State.Orders == nil {
		r.Gaps = append(r.Gaps, InspectionGap{path, "incomplete", "canonical schema or orders are missing"})
	}
	r.Snapshot, r.Status = &snapshot, "observed"
	return r
}

func readWorkspaceSources(r *WorkspaceInspection) {
	root := filepath.Join(r.ProjectDir, ".noodle")
	add := func(path, kind, session string) {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			r.Gaps = append(r.Gaps, InspectionGap{Path: path, Code: "unreadable", Detail: err.Error()})
			return
		}
		if !info.Mode().IsRegular() {
			r.Gaps = append(r.Gaps, InspectionGap{Path: path, Code: "unsafe_source", Detail: "source is not a regular file"})
			return
		}
		r.Sources = append(r.Sources, InspectionSource{Path: path, Kind: kind, SessionID: session})
	}
	for _, name := range []string{"orders.json", "orders-next.json", "pending-review.json", "tickets.json", "status.json", "control.ndjson", "control-ack.ndjson"} {
		add(filepath.Join(root, name), "workspace_record", "")
	}
	sessions := filepath.Join(root, "sessions")
	entries, err := os.ReadDir(sessions)
	if err != nil {
		if !os.IsNotExist(err) {
			r.Gaps = append(r.Gaps, InspectionGap{Path: sessions, Code: "unreadable", Detail: err.Error()})
		}
		return
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			r.Gaps = append(r.Gaps, InspectionGap{Path: filepath.Join(sessions, entry.Name()), Code: "unsafe_source", Detail: "session directory is a symbolic link"})
			continue
		}
		if !entry.IsDir() {
			continue
		}
		for _, name := range []string{"events.ndjson", "meta.json", "spawn.json", "process.json", "prompt.txt", "input.txt"} {
			add(filepath.Join(sessions, entry.Name(), name), "session_record", entry.Name())
		}
	}
}
