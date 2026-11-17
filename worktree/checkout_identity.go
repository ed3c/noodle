package worktree

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/worktree/archive"
)

// ReadCheckoutIdentity identifies a linked registration, never a reusable name.
// Legacy registrations are held rather than silently enrolled during cleanup.
func (a *App) ReadCheckoutIdentity(path string) (archive.CheckoutIdentity, error) {
	return a.custodyIdentity(path, true)
}

func (a *App) custodyIdentity(path string, tokenRequired bool) (archive.CheckoutIdentity, error) {
	var id archive.CheckoutIdentity
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || resolved != path {
		return id, fmt.Errorf("checkout path is missing, noncanonical or symlinked: %s", path)
	}
	id.Path = path
	id.Repo, err = a.gitOutput("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || (a.CommonDir != "" && a.CommonDir != id.Repo) {
		return id, fmt.Errorf("checkout original Git owner differs: %v", err)
	}
	info, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil || !info.Mode().IsRegular() {
		return id, fmt.Errorf("checkout linked Git metadata is unavailable: %v", err)
	}
	common, err := a.gitOutput("-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common != id.Repo {
		return id, fmt.Errorf("checkout belongs to another Git owner: %v", err)
	}
	id.Registration, err = a.gitOutput("-C", path, "rev-parse", "--absolute-git-dir")
	if err != nil || filepath.Dir(id.Registration) != filepath.Join(id.Repo, "worktrees") {
		return id, fmt.Errorf("checkout is not a linked registration: %v", err)
	}
	back, err := os.ReadFile(filepath.Join(id.Registration, "gitdir"))
	if err != nil || strings.TrimSpace(string(back)) != filepath.Join(path, ".git") {
		return id, fmt.Errorf("checkout registration backlink differs: %v", err)
	}
	id.Head, err = a.gitOutput("-C", path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return id, err
	}
	id.Ref, err = a.gitOutput("-C", path, "symbolic-ref", "HEAD")
	if err != nil || !strings.HasPrefix(id.Ref, "refs/heads/") {
		return id, fmt.Errorf("checkout retained branch is unavailable: %v", err)
	}
	if !tokenRequired {
		return id, nil
	}
	marker := filepath.Join(id.Registration, "noodle-incarnation.json")
	info, err = os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() {
		return id, fmt.Errorf("checkout incarnation missing or nonregular: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		return id, err
	}
	if err = archive.DecodeCheckoutJSON(raw, &id.Incarnation); err != nil {
		return id, err
	}
	token, err := hex.DecodeString(id.Incarnation)
	if err != nil || len(token) != 32 || strings.ToLower(id.Incarnation) != id.Incarnation {
		return id, fmt.Errorf("checkout incarnation is invalid")
	}
	return id, nil
}
