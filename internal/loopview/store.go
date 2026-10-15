package loopview

import (
	"bytes"
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

func Load(dir string) (View, error) {
	view, err := Read(Path(dir))
	if os.IsNotExist(err) {
		return Empty(), nil
	}
	return view, err
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
	before, err := json.Marshal(current)
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
	if err := validateImport(current); err != nil {
		return false, 0, err
	}
	data, err := json.Marshal(current)
	if err != nil {
		return false, 0, err
	}
	if len(data) > MaxBytes {
		return false, 0, fmt.Errorf("merged loop registry exceeds %d bytes", MaxBytes)
	}
	if bytes.Equal(before, data) {
		return false, len(current.Runs), nil
	}
	if err := filex.WriteFileAtomicDurable(Path(dir), data); err != nil {
		return false, 0, err
	}
	return true, len(current.Runs), nil
}
