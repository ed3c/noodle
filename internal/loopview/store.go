package loopview

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

func Path(dir string) string { return filepath.Join(dir, "loop-views.json") }

func Read(path string) (View, error) {
	f, err := os.Open(path)
	if err != nil {
		return View{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return View{}, err
	}
	return Decode(data)
}

// Load expands a committed projection for import validation only. HTTP callers use
// LoadSummary and individual detail readers so unrelated bodies stay unloaded.
func Load(dir string) (View, error) {
	view, err := LoadSummary(dir)
	if err != nil {
		return View{}, err
	}
	for i, run := range view.Runs {
		view.Runs[i], err = RunDetail(dir, run)
		if err != nil {
			return View{}, err
		}
	}
	if view.Graph != nil {
		for i, repo := range view.Graph.Repositories {
			view.Graph.Repositories[i], err = RepositoryDetail(dir, repo)
			if err != nil {
				return View{}, err
			}
		}
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return View{}, err
	}
	return decodeView(raw, false)
}

// Import serializes registry updates without acquiring the Host lifecycle lock.
// A run can be refreshed, but its repository and mode cannot be reassigned.
func Import(dir string, incoming View) (bool, int, error) {
	if err := validateImport(incoming); err != nil {
		return false, 0, err
	}
	if len(incoming.Resources) != 0 {
		return false, 0, fmt.Errorf("top-level resources are Host observations, not import data")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, 0, err
	}
	lock, err := lockfile.TryLock(filepath.Join(dir, "loop-views.lock"))
	if err != nil {
		return false, 0, err
	}
	defer lock.Close()
	current, err := Load(dir)
	if err != nil {
		return false, 0, err
	}
	before, err := os.ReadFile(Path(dir))
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return false, 0, err
	}
	byID := map[string]Run{}
	for _, run := range current.Runs {
		byID[run.ID] = run
	}
	for _, run := range incoming.Runs {
		if old, ok := byID[run.ID]; ok && (old.Repository != run.Repository || old.Mode != run.Mode) {
			return false, 0, fmt.Errorf("run %q repository or mode changed", run.ID)
		}
		byID[run.ID] = run
	}
	current.Runs = make([]Run, 0, len(byID))
	for _, run := range byID {
		current.Runs = append(current.Runs, run)
	}
	sort.Slice(current.Runs, func(i, j int) bool { return current.Runs[i].ID < current.Runs[j].ID })
	if err := mergeGraph(&current, incoming); err != nil {
		return false, 0, err
	}
	current = referencedArtifacts(current)
	rawCurrent, err := json.Marshal(current)
	if err != nil {
		return false, 0, err
	}
	if normalized, err := decodeView(rawCurrent, false); err != nil {
		return false, 0, err
	} else {
		current = normalized
	}
	summary, data, err := storeProjection(dir, current)
	if err != nil {
		return false, 0, err
	}
	// Keep the exact previous registry and accepted incoming bodies before removing
	// redundant inline copies. ArtifactContent still verifies the original source.
	if hasInlineArtifacts(incoming) {
		raw, err := json.Marshal(incoming)
		if err != nil {
			return false, 0, err
		}
		if err := archiveView(dir, raw); err != nil {
			return false, 0, err
		}
	}
	if bytes.Equal(before, data) {
		return false, len(current.Runs), nil
	}
	if err := retainVersion(dir, summary, data); err != nil {
		return false, 0, err
	}
	if err := filex.WriteFileAtomicDurable(Path(dir), data); err != nil {
		return false, 0, err
	}
	return true, len(current.Runs), nil
}

func hasInlineArtifacts(view View) bool {
	if view.Graph != nil {
		for _, artifact := range view.Graph.Artifacts {
			if artifact.Content != nil {
				return true
			}
		}
	}
	return false
}

func referencedArtifacts(view View) View {
	if view.Graph != nil {
		graph := *view.Graph
		graph.Artifacts = append([]Artifact{}, graph.Artifacts...)
		for i := range graph.Artifacts {
			graph.Artifacts[i].Content = nil
		}
		view.Graph = &graph
	}
	return view
}

func archiveView(dir string, data []byte) error {
	path := filepath.Join(dir, "loop-view-history", fmt.Sprintf("%x.json", sha256.Sum256(data)))
	if previous, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(previous, data) {
			return fmt.Errorf("loop view history content changed: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := filex.WriteFileAtomicDurable(path, data); err != nil {
		return err
	}
	return filex.SyncDir(dir)
}
