package archive

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type CommitRecipe struct {
	Paths           []string          `json:"paths"`
	Validation      [][]string        `json:"validation"`
	NoModelReviewed bool              `json:"no_model_reviewed"`
	HookHashes      map[string]string `json:"hook_sha256"`
}

type Config struct {
	Roots              []string                `json:"roots"`
	ArchiveRoot        string                  `json:"archive_root"`
	BlobRoot           string                  `json:"blob_root"`
	StateDir           string                  `json:"state_dir"`
	Codec              string                  `json:"zstd"`
	Protect            []string                `json:"protect"`
	MaxItems           int                     `json:"max_items"`
	ItemTimeoutSeconds int                     `json:"item_timeout_seconds"`
	ExternalOwnerRoots []string                `json:"external_owner_roots"`
	Recipes            map[string]CommitRecipe `json:"commit_recipes"`
}

func DefaultConfigPath() string {
	if p := os.Getenv("NOODLE_WORKTREE_ARCHIVE_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "noodle", "worktree-archive.json")
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return parseConfig(b)
}

func parseConfig(b []byte) (Config, error) {
	var c Config
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	if len(c.Roots) == 0 || c.MaxItems < 1 || c.ItemTimeoutSeconds < 1 {
		return c, errors.New("archive roots or processing limits are missing")
	}
	for _, p := range append(append([]string{}, c.Roots...), c.ArchiveRoot, c.BlobRoot, c.StateDir) {
		if err := canonical(p); err != nil {
			return c, err
		}
	}
	if !filepath.IsAbs(c.Codec) {
		return c, errors.New("absolute zstd executable is missing")
	}
	for _, root := range c.Roots {
		for _, p := range []string{c.ArchiveRoot, c.BlobRoot, c.StateDir} {
			if within(root, p) || within(p, root) {
				return c, errors.New("archive storage overlaps a managed root")
			}
		}
	}
	return c, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || safeRelative(rel))
}
func (c Config) Managed(path string) bool {
	for _, p := range c.Protect {
		if within(p, path) || within(path, p) {
			return false
		}
	}
	for _, p := range c.Roots {
		if path != p && within(p, path) {
			return true
		}
	}
	return false
}

func rootKey(path string) string { h := sha256.Sum256([]byte(path)); return hex.EncodeToString(h[:]) }

func git(root string, args ...string) (string, error) {
	cmd, cancel := timedCommand(30*time.Second, "git", append([]string{"--no-optional-locks", "-c", "core.fsync=all", "-c", "core.hooksPath=" + os.DevNull, "-C", root}, args...)...)
	defer cancel()
	var out, stderr strings.Builder
	cmd.Env = []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(strings.SplitN(entry, "=", 2)[0]), "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return out.String(), nil
}

func authenticate(c Config, value any) (string, error) {
	p := filepath.Join(c.StateDir, "owner.key")
	st, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return "", errors.New("owner signing key permissions are unsafe")
	}
	key, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if len(key) != 32 {
		return "", errors.New("owner signing key size differs")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	h := hmac.New(sha256.New, key)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}
