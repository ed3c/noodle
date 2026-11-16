package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The worktree owner can observe absence. It cannot classify another owner's
// data or manufacture a Test Manager inventory when that namespace exists.
func observeForeignNamespaceAbsence(s Source) ([]byte, error) {
	name := "soodles-test-requests.noindex"
	path := filepath.Join(s.Admin, name)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return nil, fmt.Errorf("foreign Test namespace HOLD; original test-manager inventory required: %s (%v)", path, err)
	}
	for rel := range s.AdminTree.Entries {
		if rel == name || strings.HasPrefix(rel, name+"/") {
			return nil, fmt.Errorf("foreign Test namespace changed during observation HOLD: %s", path)
		}
	}
	return json.Marshal(struct {
		Owner       string `json:"owner"`
		Observation string `json:"observation"`
		Repository  string `json:"repository"`
		GitDir      string `json:"git_dir"`
		CommonDir   string `json:"common_dir"`
		Namespace   string `json:"namespace"`
	}{"worktree", "observed_absent", s.Path, s.Admin, s.Common, path})
}
