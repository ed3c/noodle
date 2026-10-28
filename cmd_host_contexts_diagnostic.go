package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/filex"
)

type contextOwnerOperationError struct {
	Cause            error
	DiagnosticID     string
	DiagnosticFailed bool
}

func (e *contextOwnerOperationError) Error() string { return "original owner operation unconfirmed" }
func (e *contextOwnerOperationError) Unwrap() error { return e.Cause }

type contextOwnerDiagnostic struct {
	Schema          int                  `json:"schema"`
	Started         time.Time            `json:"started"`
	Finished        time.Time            `json:"finished"`
	Action          string               `json:"action"`
	Request         contextFileRef       `json:"request"`
	Launcher        contextFileRef       `json:"launcher"`
	Manifest        contextFileRef       `json:"manifest"`
	OriginalHost    contextOwnerIdentity `json:"original_host"`
	ExitCode        int                  `json:"exit_code"`
	TimedOut        bool                 `json:"timed_out"`
	Failure         string               `json:"failure"`
	Stderr          string               `json:"stderr"`
	StderrTruncated bool                 `json:"stderr_truncated"`
	Stdout          string               `json:"stdout"`
	StdoutTruncated bool                 `json:"stdout_truncated"`
}

func saveContextOwnerDiagnostic(hostDir string, evidence contextOwnerDiagnostic) (string, error) {
	dir := filepath.Join(hostDir, "context-control-diagnostics")
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("private process diagnostic directory is not private")
	}
	id, err := hostID()
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return "", err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if err = filex.SyncDir(dir); err != nil {
		return "", err
	}
	if err = filex.SyncDir(hostDir); err != nil {
		return "", err
	}
	return id, nil
}
