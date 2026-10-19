package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

func newHostCleanupCmd(app *App) *cobra.Command {
	var requestFile, instanceID string
	var apply bool
	cmd := &cobra.Command{Use: "cleanup", Short: "Inspect or release completed contexts through their original Host", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if apply && instanceID == "" {
			return errors.New("cleanup apply requires the instance-id from inspect")
		}
		if requestFile != "" {
			result := callHost(cmd.Context(), hostRequest{Operation: "cleanup", ProjectDir: app.projectDir, RequestFile: requestFile, Apply: apply, InstanceID: instanceID})
			output, err := boundCleanupResult(result, requestFile, apply)
			if err != nil {
				return err
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(output); err != nil {
				return err
			}
			if result.Error != "" {
				return errors.New(result.Error)
			}
			return nil
		}
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		binary, err := os.ReadFile(executable)
		if err != nil {
			return err
		}
		conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
		if err != nil {
			return fmt.Errorf("cleanup Host unavailable: %w", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
		if err = json.NewEncoder(conn).Encode(hostRequest{Operation: "cleanup_all", Apply: apply, InstanceID: instanceID, BinarySHA256: hostDigest(binary)}); err != nil {
			return err
		}
		var result map[string]any
		if err = json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&result); err != nil {
			return err
		}
		if err = json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
			return err
		}
		if result["status"] == "refused" || result["status"] == "unknown_effect" {
			return errors.New("cleanup incomplete; read the original Host before retrying")
		}
		return nil
	}}
	cmd.Flags().StringVar(&instanceID, "instance-id", "", "Original Host instance from cleanup inspection")
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Limit cleanup to the original native request")
	cmd.Flags().BoolVar(&apply, "apply", false, "Release eligible completed contexts after fresh native checks")
	return cmd
}

func boundCleanupResult(result hostResponse, requestFile string, apply bool) (any, error) {
	if result.Status != "eligible" || apply {
		return result, nil
	}
	path, err := filepath.Abs(requestFile)
	if err != nil {
		return nil, err
	}
	next := map[string]any{"owner": "noodle.swap-manager", "operation": "cleanup", "argv": []string{result.BinaryPath, "--project-dir", result.ProjectDir, "host", "cleanup", "--request-file", path, "--apply", "--instance-id", result.HostInstanceID}}
	return struct {
		hostResponse
		Next map[string]any `json:"next"`
	}{result, next}, nil
}

func (h *noodleHost) cleanupAutomaticContexts(ctx context.Context) {
	if h.blocked {
		return
	}
	for _, c := range h.contexts {
		if !c.receipt.CleanupCompleted || c.failed || c.done == nil {
			continue
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.runtime.CleanupAutomatic(cleanupCtx)
		if err == nil {
			select {
			case runErr := <-c.done:
				h.finish(c, runErr)
			case <-cleanupCtx.Done():
				err = cleanupCtx.Err()
			}
		}
		cancel()
		if err != nil && !errors.Is(err, loop.ErrReleaseBusy) {
			fmt.Fprintf(os.Stderr, "automatic context cleanup requires readback: %v\n", err)
			return
		}
	}
}

// The Host selects current identities in its actor; callers supply no PID or
// historical list. Each candidate receives the same fresh Cleanup check.
func (h *noodleHost) cleanupContexts(ctx context.Context, req hostRequest) any {
	result := map[string]any{"schema": 1, "owner": "noodle.swap-manager", "host_instance_id": h.instanceID, "status": "observed", "contexts": []hostResponse{}}
	if req.BinarySHA256 != h.binarySHA || h.blocked || (req.Apply && req.InstanceID != h.instanceID) {
		result["status"] = "refused"
		result["error"] = "current Host binary, instance or recovery binding unavailable"
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ids := make([]string, 0, len(h.contexts))
	for id := range h.contexts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	rows := make([]hostResponse, 0, len(ids))
	eligible := false
	for _, id := range ids {
		if ctx.Err() != nil {
			result["status"] = "unknown_effect"
			result["error"] = ctx.Err().Error()
			break
		}
		c := h.contexts[id]
		r := c.receipt
		response := h.handle(ctx, hostRequest{Operation: "cleanup", Apply: req.Apply,
			ProjectDir: r.ProjectDir, ContextID: r.ContextID, InstanceID: r.HostInstanceID,
			RequestID: r.RequestID, ConfigSHA256: r.ConfigSHA256, Mode: r.Mode, BinarySHA256: h.binarySHA, CleanupCompleted: r.CleanupCompleted})
		rows = append(rows, response)
		eligible = eligible || response.Status == "eligible"
		if response.Status == "unknown_effect" {
			result["status"] = "unknown_effect"
			break
		}
	}
	result["contexts"] = rows
	if !req.Apply && eligible && result["status"] == "observed" {
		result["next"] = map[string]any{"owner": "noodle.swap-manager", "operation": "cleanup", "argv": []string{h.binaryPath, "host", "cleanup", "--apply", "--instance-id", h.instanceID}}
	}
	return result
}
