package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/worktree"
)

// A local-task context integrates into its admitted control checkout. Persist
// this once so a Host restart cannot silently adopt a different branch.
func bindContextIntegration(app *worktree.App, runtimeDir string) error {
	data, err := app.Execution.Command(context.Background(), app.Root, "git", "symbolic-ref", "--short", "HEAD").Output()
	branch := strings.TrimSpace(string(data))
	if err != nil || branch == "" {
		return fmt.Errorf("local task integration checkout is detached")
	}
	path := filepath.Join(runtimeDir, "integration-branch.json")
	original, err := os.ReadFile(path)
	if err == nil {
		var binding struct {
			Branch string `json:"branch"`
			Root   string `json:"root"`
		}
		if json.Unmarshal(original, &binding) != nil || binding.Branch != branch || binding.Root != app.Root {
			return fmt.Errorf("local task integration branch drifted")
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(runtimeDir, 0700); err != nil {
			return err
		}
		value, err := json.Marshal(map[string]string{"branch": branch, "root": app.Root})
		if err != nil {
			return err
		}
		if err := filex.WriteFileAtomicDurable(path, value); err != nil {
			return err
		}
	} else {
		return err
	}
	app.IntegrationBranch = branch
	return nil
}
