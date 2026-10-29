package workspaceartifacts

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(ctx context.Context, root string, input []byte, args ...string) ([]byte, error) {
	options := []string{"-C", root, "-c", "core.autocrlf=false", "-c", "core.filemode=true", "-c", "core.fsmonitor=false", "-c", "core.quotePath=true", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.attributesFile=" + os.DevNull}
	cmd := exec.CommandContext(ctx, "git", append(options, args...)...)
	// Repository selection, alternate indexes, and injected Git configuration
	// must not be inherited from the caller's shell.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1")
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func applyGit(ctx context.Context, target, build string, patch []byte, check bool) error {
	args := []string{"--git-dir=" + filepath.Join(build, ".git"), "--work-tree=" + target, "apply", "--binary", "--whitespace=nowarn"}
	if check {
		args = append(args, "--check")
	}
	_, err := git(ctx, target, patch, append(args, "-")...)
	return err
}

func repositoryRoot(ctx context.Context, root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	out, err := git(ctx, root, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return "", err
	}
	if root != top {
		return "", fmt.Errorf("source is not the repository root: %q", root)
	}
	return root, nil
}

func externalDirectory(target, directory string) (string, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(directory))
	if err != nil {
		return "", fmt.Errorf("resolve artifact parent: %w", err)
	}
	directory = filepath.Join(parent, filepath.Base(directory))
	rel, err := filepath.Rel(target, directory)
	if err != nil {
		return "", err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return "", fmt.Errorf("artifact directory is inside target: %q", directory)
	}
	return directory, nil
}

func identity(ctx context.Context, root string) (string, string, error) {
	head, err := git(ctx, root, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", "", err
	}
	index, err := git(ctx, root, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", "", err
	}
	indexPath := strings.TrimSpace(string(index))
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(root, indexPath)
	}
	body, err := os.ReadFile(indexPath)
	if err != nil {
		return "", "", fmt.Errorf("read target index: %w", err)
	}
	return strings.TrimSpace(string(head)), digest(body), nil
}

// Hash raw bytes and construct trees directly. git add could execute repository
// clean filters or normalize line endings, invalidating the captured bytes.
func writeTree(ctx context.Context, root string, entries map[string]Entry, data contents) (string, error) {
	if _, err := git(ctx, root, nil, "read-tree", "--empty"); err != nil {
		return "", err
	}
	var index bytes.Buffer
	for _, name := range names(entries) {
		object, err := git(ctx, root, data[name], "hash-object", "-w", "--stdin", "--no-filters")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&index, "%s %s\t%s\x00", entries[name].Mode, strings.TrimSpace(string(object)), name)
	}
	if _, err := git(ctx, root, index.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	out, err := git(ctx, root, nil, "write-tree")
	return strings.TrimSpace(string(out)), err
}
