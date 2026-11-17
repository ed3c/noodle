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

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

type ControlSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type TaskControl struct {
	Preparation admission.PreparationRef `json:"preparation"`
	Schema      int                      `json:"schema"`
	Status      string                   `json:"status"`
	Task        ControlSource            `json:"task"`
	Repository  string                   `json:"repository"`
	ControlRoot string                   `json:"control_root"`
	Branch      string                   `json:"branch"`
	Head        string                   `json:"head"`
}

// PlanTaskControl fixes the original source and all checkout paths without
// creating a directory, journal or Git worktree.
func PlanTaskControl(ctx context.Context, hostRoot, taskFile string) (TaskControl, error) {
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
	repository, err := filepath.EvalSymlinks(task.Repository)
	if err != nil {
		return result, err
	}
	actual, err := taskControlGit(ctx, repository, "rev-parse", "--show-toplevel")
	if err != nil || actual != repository {
		return result, errors.New("task source is not the exact Git root")
	}
	base := task.Base
	if base == "" {
		base = "origin/HEAD"
	}
	head, err := taskControlGit(ctx, repository, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return result, err
	}
	digest := sha256.Sum256(raw)
	ref := ControlSource{Path: taskFile, SHA256: hex.EncodeToString(digest[:])}
	identity := sha256.Sum256([]byte(hostRoot + "\x00" + ref.SHA256))
	name := "noodle-control-" + hex.EncodeToString(identity[:])[:20]
	directory := filepath.Join(hostRoot, "controls", name)
	return TaskControl{Schema: 2, Status: "creating", Task: ref, Repository: repository,
		ControlRoot: filepath.Join(directory, "checkout"), Branch: name, Head: head}, nil
}

func taskControlGit(ctx context.Context, root string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// PrepareTaskControl retains the old readback entry. New effects require the
// original native producer's machine reservation.
func PrepareTaskControl(ctx context.Context, hostRoot, taskFile string) (TaskControl, error) {
	return PrepareTaskControlWithAdmission(ctx, hostRoot, taskFile, nil)
}

func PrepareTaskControlWithAdmission(ctx context.Context, hostRoot, taskFile string, beforeEffects func(context.Context, TaskControl) (admission.PreparationRef, error)) (TaskControl, error) {
	result, err := PlanTaskControl(ctx, hostRoot, taskFile)
	if err != nil {
		return result, err
	}
	directory := filepath.Dir(result.ControlRoot)
	path := filepath.Join(directory, "control.json")
	priorRaw, priorErr := os.ReadFile(path)
	if priorErr == nil {
		var prior TaskControl
		if json.Unmarshal(priorRaw, &prior) != nil || (prior.Schema != 1 && prior.Schema != 2) || prior.Task != result.Task || prior.Repository != result.Repository || prior.ControlRoot != result.ControlRoot || prior.Branch != result.Branch {
			return result, errors.New("original task control binding changed")
		}
		result = prior
	} else if !os.IsNotExist(priorErr) {
		return result, priorErr
	}
	if priorErr != nil || result.Schema == 2 {
		if beforeEffects == nil {
			return result, fmt.Errorf("%w: original task preparation reservation missing", admission.ErrDeferred)
		}
		ref, e := beforeEffects(ctx, result)
		if e != nil {
			return result, e
		}
		if ref.ID == "" || ref.SHA256 == "" || (result.Preparation.ID != "" && result.Preparation != ref) {
			return result, errors.New("original task preparation reference differs")
		}
		result.Preparation = ref
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return result, err
	}
	lock, err := lockfile.Acquire(ctx, filepath.Join(directory, "control.lock"))
	if err != nil {
		return result, err
	}
	defer lock.Close()
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
		expected := result
		expected.Status = prior.Status
		if prior != expected || (prior.Status != "creating" && prior.Status != "prepared") {
			return result, errors.New("original task control binding changed")
		}
		result = prior
	} else if !os.IsNotExist(err) {
		return result, err
	} else {
		if err = save(); err != nil {
			return result, err
		}
		if _, err = taskControlGit(ctx, result.Repository, "worktree", "add", "-b", result.Branch, result.ControlRoot, result.Head); err != nil {
			return result, err
		}
	}
	root, err := taskControlGit(ctx, result.ControlRoot, "rev-parse", "--show-toplevel")
	if err != nil || root != result.ControlRoot {
		return result, errors.New("original control creation requires Git readback; no creation replayed")
	}
	branch, err := taskControlGit(ctx, result.ControlRoot, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != result.Branch {
		return result, errors.New("original control branch changed")
	}
	if _, err = taskControlGit(ctx, result.ControlRoot, "merge-base", "--is-ancestor", result.Head, "HEAD"); err != nil {
		return result, errors.New("original control source ancestry changed")
	}
	if result.Status == "prepared" {
		return result, nil
	}
	result.Status = "prepared"
	return result, save()
}
