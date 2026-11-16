package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// The original profile owns this path. A cleanup caller cannot supply a disk
// location, and no operation in this file removes or traverses VM contents.
type openShellVMStorage struct {
	Config SessionReadbackFileRef
	Root   string
}

func openShellVMStorageForProfile(profile OpenShellProfile) (openShellVMStorage, error) {
	if err := VerifyOpenShellProfile(profile); err != nil {
		return openShellVMStorage{}, err
	}
	source := profile.Gateway.Binding.ConfigSource
	expected := profile.sourceFiles[source]
	if profile.Gateway.ComputeDriver != "vm" || expected == "" {
		return openShellVMStorage{}, fmt.Errorf("original VM driver configuration is not profile-bound")
	}
	ref, err := ReadSessionReadbackFile(source, nil)
	if err != nil {
		return openShellVMStorage{}, err
	}
	if ref.SHA256 != expected || ref.Path != source {
		return openShellVMStorage{}, fmt.Errorf("original VM configuration changed")
	}
	storage := openShellVMStorage{Config: ref}
	storage.Root, err = readOpenShellVMRoot(ref)
	if err != nil {
		return openShellVMStorage{}, err
	}
	return storage, storage.verify()
}

func readOpenShellVMRoot(ref SessionReadbackFileRef) (string, error) {
	before, err := ReadSessionReadbackFile(ref.Path, nil)
	if err != nil {
		return "", err
	}
	if before != ref {
		return "", fmt.Errorf("original VM configuration bytes changed")
	}
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return "", err
	}
	var config struct {
		OpenShell struct {
			Gateway struct {
				ComputeDriver string `toml:"compute_driver"`
			} `toml:"gateway"`
			Drivers struct {
				VM struct {
					StateDir string `toml:"state_dir"`
				} `toml:"vm"`
			} `toml:"drivers"`
		} `toml:"openshell"`
	}
	if _, err := toml.Decode(string(data), &config); err != nil {
		return "", fmt.Errorf("decode original VM configuration: %w", err)
	}
	root := config.OpenShell.Drivers.VM.StateDir
	if config.OpenShell.Gateway.ComputeDriver != "vm" || !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.ContainsRune(root, 0) {
		return "", fmt.Errorf("original VM state directory is missing or invalid")
	}
	after, err := ReadSessionReadbackFile(ref.Path, nil)
	if err != nil {
		return "", err
	}
	if after != ref {
		return "", fmt.Errorf("original VM configuration changed during read")
	}
	return root, nil
}

func (storage openShellVMStorage) verify() error {
	root, err := readOpenShellVMRoot(storage.Config)
	if err != nil {
		return err
	}
	if root != storage.Root {
		return fmt.Errorf("original VM state directory differs")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if canonical != root || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("original VM state root is not a canonical directory")
	}
	return nil
}

func (storage openShellVMStorage) sandboxPresent(sandboxID string) (bool, error) {
	if sandboxID == "" || filepath.Base(sandboxID) != sandboxID || sandboxID == "." || sandboxID == ".." || strings.ContainsRune(sandboxID, 0) {
		return false, fmt.Errorf("original VM storage identity is invalid")
	}
	if err := storage.verify(); err != nil {
		return false, err
	}
	root := filepath.Join(storage.Root, "sandboxes")
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, err
	}
	if canonical != root {
		return false, fmt.Errorf("original VM sandbox directory is not canonical")
	}
	info, err := os.Lstat(filepath.Join(root, sandboxID))
	if os.IsNotExist(err) {
		return false, storage.verify()
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("original VM state is not an owned directory")
	}
	return true, storage.verify()
}
