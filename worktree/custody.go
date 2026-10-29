package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/execution"
)

// StorageSource binds disposable aliases to the repository's durable bytes.
type StorageSource struct {
	Project     string `json:"project"`
	CommonDir   string `json:"common_dir"`
	StorageRoot string `json:"storage_root"`
}

type Custody struct {
	Logical   string `json:"logical"`
	Physical  string `json:"physical"`
	CommonDir string `json:"common_dir"`
	GitAdmin  string `json:"git_admin"`
}

func ReadCustody(ctx context.Context, e *execution.Context, logical string) (Custody, error) {
	r := Custody{Logical: logical}
	separator := string(filepath.Separator) + ".worktrees" + string(filepath.Separator)
	boundary := strings.LastIndex(logical, separator)
	if !filepath.IsAbs(logical) || filepath.Clean(logical) != logical || boundary < 0 {
		return r, fmt.Errorf("worktree is outside original task root")
	}
	sourceRoot, err := filepath.EvalSymlinks(logical[:boundary])
	if err != nil || sourceRoot != e.Root() {
		return r, fmt.Errorf("worktree is outside original task root")
	}
	relative := logical[boundary+len(separator):]
	canonicalLogical := filepath.Join(e.Root(), ".worktrees", relative)
	physical, err := filepath.EvalSymlinks(logical)
	if err != nil {
		return r, err
	}
	r.Physical = physical
	git := func(root string, args ...string) (string, error) {
		raw, err := e.Command(ctx, root, "git", args...).Output()
		return strings.TrimSpace(string(raw)), err
	}
	r.CommonDir, err = git(e.Root(), "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return r, err
	}
	common, err := git(physical, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common != r.CommonDir {
		return r, fmt.Errorf("worker Git common owner changed")
	}
	actual, err := git(physical, "rev-parse", "--show-toplevel")
	if err != nil || actual != physical {
		return r, fmt.Errorf("worker physical Git root changed")
	}
	r.GitAdmin, err = git(physical, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return r, err
	}
	if !strings.HasPrefix(r.GitAdmin, filepath.Join(r.CommonDir, "worktrees")+string(filepath.Separator)) {
		return r, fmt.Errorf("worker Git admin owner changed")
	}
	if physical != canonicalLogical {
		runtimeDir, err := filepath.EvalSymlinks(filepath.Join(e.Root(), ".noodle"))
		if err != nil {
			return r, err
		}
		storage := filepath.Dir(runtimeDir)
		raw, err := os.ReadFile(filepath.Join(storage, "source.json"))
		if err != nil {
			return r, err
		}
		var source StorageSource
		if json.Unmarshal(raw, &source) != nil || source.Project != e.Root() || source.CommonDir != r.CommonDir || source.StorageRoot != storage {
			return r, fmt.Errorf("original storage source binding changed")
		}
		if physical != filepath.Join(storage, ".worktrees", relative) {
			return r, fmt.Errorf("worker alias escapes original storage")
		}
		sum := sha256.Sum256([]byte(canonicalLogical))
		raw, err = os.ReadFile(filepath.Join(storage, "worktree-bindings", hex.EncodeToString(sum[:])+".json"))
		if err != nil {
			return r, err
		}
		var binding map[string]string
		if json.Unmarshal(raw, &binding) != nil || binding["original"] != canonicalLogical || binding["physical"] != physical || binding["git_admin"] != r.GitAdmin {
			return r, fmt.Errorf("original worker custody binding changed")
		}
	}
	back, err := os.ReadFile(filepath.Join(r.GitAdmin, "gitdir"))
	if err != nil {
		return r, err
	}
	backPath, backErr := filepath.EvalSymlinks(strings.TrimSpace(string(back)))
	if backErr != nil || backPath != filepath.Join(physical, ".git") {
		return r, fmt.Errorf("worker Git backpointer changed")
	}
	return r, nil
}
