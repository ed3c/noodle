package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// Native file transfer is name based. The dedicated gateway contract excludes
// external lifecycle writers; fresh original-ID reads fence each transfer.
func (s *openShellSession) transfer(direction, source, destination string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-s.stopRequested:
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := s.worker.Readback(ctx, s.custody); err != nil {
		return err
	}
	if s.worker.Record().Boundary != OpenShellBoundaryReady {
		return fmt.Errorf("openshell file transfer requires original Ready VM")
	}
	profile := s.owner.config.Profile
	if err := VerifyOpenShellClient(profile); err != nil {
		return err
	}
	args := []string{"--gateway", profile.GatewayName, "--workspace", s.worker.Record().Identity.Sandbox.Workspace, "--color", "never", "sandbox", direction}
	if direction == "upload" {
		args = append(args, "--no-git-ignore")
	} else if direction != "download" {
		return fmt.Errorf("openshell transfer direction invalid")
	}
	args = append(args, s.worker.Record().Identity.SandboxName, source, destination)
	log, err := os.CreateTemp(s.dir, "transfer-"+direction+"-*.log")
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, profile.CLI, args...)
	cmd.Env = append([]string(nil), profile.Environment...)
	cmd.Dir = s.dir
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("openshell %s failed; original evidence %s: %w", direction, filepath.Base(log.Name()), err)
	}
	if err := VerifyOpenShellClient(profile); err != nil {
		return err
	}
	if err = s.worker.Readback(ctx, s.custody); err != nil {
		return err
	}
	if s.worker.Record().Boundary != OpenShellBoundaryReady {
		return fmt.Errorf("openshell VM changed during file transfer")
	}
	return nil
}

func (s *openShellSession) downloadArchive(name, destination string) error {
	staging, err := os.MkdirTemp(s.dir, "download-")
	if err != nil {
		return err
	}
	if err = s.transfer("download", "/sandbox/"+name, staging); err != nil {
		return err
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != name {
		return fmt.Errorf("openshell download contains unexpected archive entries")
	}
	return workspaceartifacts.Extract(context.Background(), filepath.Join(staging, name), destination)
}
