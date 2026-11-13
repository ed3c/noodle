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
	"github.com/poteto/noodle/internal/filex"
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
	return readCustody(ctx, e, logical, true)
}

func readCustody(ctx context.Context, e *execution.Context, logical string, requireBinding bool) (Custody, error) {
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
		if requireBinding {
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

// EnsureCustody resumes original worker preparation without dispatching a session.
func (a *App) EnsureCustody(name string) error {
	e, err := execution.New(a.Root, a.execution().Environ())
	if err != nil {
		return err
	}
	logical := WorktreePath(e.Root(), name)
	custody, err := readCustody(context.Background(), e, logical, false)
	if err != nil {
		return err
	}
	if custody.Physical == logical {
		return nil
	}
	runtimeDir, err := filepath.EvalSymlinks(filepath.Join(a.Root, ".noodle"))
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]string{"original": logical, "physical": custody.Physical, "git_admin": custody.GitAdmin})
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(logical))
	directory := filepath.Join(filepath.Dir(runtimeDir), "worktree-bindings")
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	path := filepath.Join(directory, hex.EncodeToString(sum[:])+".json")
	before, err := os.ReadFile(path)
	if err == nil {
		if string(before) != string(data) {
			return fmt.Errorf("original worker custody binding changed")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = filex.WriteFileAtomicDurable(path, data); err != nil {
		return err
	}
	return filex.SyncDir(filepath.Dir(directory))
}
