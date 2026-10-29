package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
)

// RuntimeSource is written by the Host under the project lock. It retains the
// selected source for offline review and cleanup without copying credentials.
type RuntimeSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func ReadExplicitSource(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("config source is not a clean absolute path")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config source: %w", err)
	}
	if real != path {
		return nil, fmt.Errorf("config source contains a symbolic link")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("config source is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, fmt.Errorf("config source changed while opening")
	}
	return io.ReadAll(f)
}

func runtimeSourcePath(project string) string {
	return filepath.Join(project, ".noodle", "host-config.json")
}

// Runtime aliases are produced by Host storage custody. External selected
// config sources still use ReadExplicitSource and reject all symbolic links.
func runtimeBindingPath(project string) (string, error) {
	logical := filepath.Join(project, ".noodle")
	physical, err := filepath.EvalSymlinks(logical)
	if errors.Is(err, os.ErrNotExist) {
		if _, aliasErr := os.Lstat(logical); aliasErr == nil {
			return "", fmt.Errorf("original runtime config custody target is missing")
		} else if !errors.Is(aliasErr, os.ErrNotExist) {
			return "", aliasErr
		}
		return runtimeSourcePath(project), nil
	}
	if err != nil {
		return "", err
	}
	if physical != logical {
		storage := filepath.Dir(physical)
		raw, err := ReadExplicitSource(filepath.Join(storage, "source.json"))
		if err != nil {
			return "", err
		}
		var source struct {
			Project     string `json:"project"`
			StorageRoot string `json:"storage_root"`
		}
		if json.Unmarshal(raw, &source) != nil || source.Project != project || source.StorageRoot != storage || physical != filepath.Join(storage, ".noodle") {
			return "", fmt.Errorf("runtime config custody binding changed")
		}
	}
	return filepath.Join(physical, "host-config.json"), nil
}

func SaveRuntimeSource(project string, source RuntimeSource) (bool, error) {
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		return false, err
	}
	path, err := runtimeBindingPath(project)
	if err != nil {
		return false, err
	}
	if source.Path == "" {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		return false, fmt.Errorf("project has an explicit Host config source; default config cannot replace it")
	}
	if len(source.SHA256) != 64 {
		return false, fmt.Errorf("runtime config binding is incomplete")
	}
	data, err := json.Marshal(source)
	if err != nil {
		return false, err
	}
	existing, err := ReadExplicitSource(path)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return false, fmt.Errorf("runtime config source binding differs")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return false, fmt.Errorf("runtime config binding is not an absent regular path")
	}
	return true, filex.WriteFileAtomicDurable(path, data)
}

// RollbackRuntimeSource removes only a newly installed reference after a
// known pre-runtime failure. The caller still holds the project lock.
func RollbackRuntimeSource(project string, source RuntimeSource) error {
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		return err
	}
	path, err := runtimeBindingPath(project)
	if err != nil {
		return err
	}
	data, err := ReadExplicitSource(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, check := os.Lstat(path); os.IsNotExist(check) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	want, err := json.Marshal(source)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, want) {
		return fmt.Errorf("runtime config binding changed before rollback")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return filex.SyncDir(filepath.Dir(path))
}

// LoadRuntime preserves the Host's explicit selection for consumers that run
// after the Host exits. Missing or changed selected bytes never use defaults.
func LoadRuntime(project string) (Config, ValidationResult, error) {
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		return Config{}, ValidationResult{}, err
	}
	path, err := runtimeBindingPath(project)
	if err != nil {
		return Config{}, ValidationResult{}, err
	}
	_, err = os.Lstat(path)
	if os.IsNotExist(err) {
		return Load(filepath.Join(project, DefaultConfigPath))
	}
	if err != nil {
		return Config{}, ValidationResult{}, err
	}
	data, err := ReadExplicitSource(path)
	if err != nil {
		return Config{}, ValidationResult{}, err
	}
	var source RuntimeSource
	if err := json.Unmarshal(data, &source); err != nil {
		return Config{}, ValidationResult{}, fmt.Errorf("read runtime config binding: %w", err)
	}
	if len(source.SHA256) != 64 {
		return Config{}, ValidationResult{}, fmt.Errorf("runtime config binding is incomplete")
	}
	data, err = ReadExplicitSource(source.Path)
	if err != nil {
		return Config{}, ValidationResult{}, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != source.SHA256 {
		return Config{}, ValidationResult{}, fmt.Errorf("runtime config source changed")
	}
	cfg, diagnostics, err := parseWithDiagnostics(data)
	if err != nil {
		return Config{}, ValidationResult{}, err
	}
	validation := Validate(cfg)
	validation.Diagnostics = append(diagnostics, validation.Diagnostics...)
	return cfg, validation, nil
}
