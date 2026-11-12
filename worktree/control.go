package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

type ControlSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type TaskControl struct {
	Schema      int           `json:"schema"`
	Status      string        `json:"status"`
	Task        ControlSource `json:"task"`
	Repository  string        `json:"repository"`
	ControlRoot string        `json:"control_root"`
	Branch      string        `json:"branch"`
	Head        string        `json:"head"`
}

// PrepareTaskControl owns the admission checkout; it never switches or edits the source tree.
func PrepareTaskControl(ctx context.Context, hostRoot, taskFile string) (TaskControl, error) {
	var result TaskControl
	if !filepath.IsAbs(hostRoot) || !filepath.IsAbs(taskFile) {
		return result, errors.New("Host root or task source is not absolute")
	}
	var err error
	hostRoot, err = filepath.EvalSymlinks(hostRoot)
	if err != nil {
		return result, err
	}
	taskFile, err = filepath.EvalSymlinks(taskFile)
	if err != nil {
		return result, err
	}
	raw, err := os.ReadFile(taskFile)
	if err != nil {
		return result, err
	}
	var task struct {
		Repository string `json:"repository"`
		Base       string `json:"base_ref"`
	}
	if err = json.Unmarshal(raw, &task); err != nil {
		return result, err
	}
	if !filepath.IsAbs(task.Repository) {
		return result, errors.New("task repository is not absolute")
	}
	run := func(root string, args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = root
		output, commandErr := command.CombinedOutput()
		if commandErr != nil {
			return "", fmt.Errorf("git %s: %w: %s", args[0], commandErr, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	repository, err := filepath.EvalSymlinks(task.Repository)
	if err != nil {
		return result, err
	}
	actual, err := run(repository, "rev-parse", "--show-toplevel")
	if err != nil || actual != repository {
		return result, errors.New("task source is not the exact Git root")
	}
	base := task.Base
	if base == "" {
		base = "origin/HEAD"
	}
	head, err := run(repository, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return result, err
	}
	digest := sha256.Sum256(raw)
	ref := ControlSource{Path: taskFile, SHA256: hex.EncodeToString(digest[:])}
	identity := sha256.Sum256([]byte(hostRoot + "\x00" + ref.SHA256))
	name := "noodle-control-" + hex.EncodeToString(identity[:])[:20]
	directory := filepath.Join(hostRoot, "controls", name)
	if err = os.MkdirAll(directory, 0700); err != nil {
		return result, err
	}
	lock, err := lockfile.Acquire(ctx, filepath.Join(directory, "control.lock"))
	if err != nil {
		return result, err
	}
	defer lock.Close()
	path := filepath.Join(directory, "control.json")
	result = TaskControl{Schema: 1, Status: "creating", Task: ref, Repository: repository,
		ControlRoot: filepath.Join(directory, "checkout"), Branch: name, Head: head}
	save := func() error {
		data, e := json.Marshal(result)
		if e != nil {
			return e
		}
		return filex.WriteFileAtomicDurable(path, data)
	}
	before, err := os.ReadFile(path)
	if err == nil {
		var prior TaskControl
		if err = json.Unmarshal(before, &prior); err != nil {
			return result, err
		}
		if prior.Schema != 1 || prior.Task != ref || prior.Repository != repository || prior.ControlRoot != result.ControlRoot || prior.Branch != name {
			return result, errors.New("original task control binding changed")
		}
		result = prior
	} else if !os.IsNotExist(err) {
		return result, err
	} else {
		if err = save(); err != nil {
			return result, err
		}
		if _, err = run(repository, "worktree", "add", "-b", name, result.ControlRoot, head); err != nil {
			return result, err
		}
	}
	root, err := run(result.ControlRoot, "rev-parse", "--show-toplevel")
	if err != nil || root != result.ControlRoot {
		return result, errors.New("original control creation requires Git readback; no creation replayed")
	}
	branch, err := run(result.ControlRoot, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != name {
		return result, errors.New("original control branch changed")
	}
	if _, err = run(result.ControlRoot, "merge-base", "--is-ancestor", result.Head, "HEAD"); err != nil {
		return result, errors.New("original control source ancestry changed")
	}
	result.Status = "prepared"
	return result, save()
}
