package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/poteto/noodle/internal/admission"
	"github.com/spf13/cobra"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

func newHostResidentsCmd(app *App) *cobra.Command {
	parent := &cobra.Command{Use: "residents", Short: "Manage shared Host resident leases and inspect external ownership gaps"}
	for _, operation := range []string{"inspect", "reconcile", "acquire", "release"} {
		var specFile, id string
		child := &cobra.Command{Use: operation, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			req := hostRequest{Operation: "resident_" + operation, ProjectDir: app.projectDir, ResidentID: id}
			if operation == "acquire" {
				data, err := os.ReadFile(specFile)
				if err != nil {
					return err
				}
				dec := json.NewDecoder(bytes.NewReader(data))
				dec.DisallowUnknownFields()
				var spec admission.ResidentSpec
				if err = dec.Decode(&spec); err != nil {
					return err
				}
				if err = dec.Decode(new(any)); err != io.EOF {
					return errors.New("resident spec contains trailing JSON")
				}
				req.ResidentSpec = &spec
			}
			dir, err := hostDirectory()
			if err != nil {
				return err
			}
			conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
			if err != nil {
				return fmt.Errorf("resident Host unavailable: %w", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
			if err = json.NewEncoder(conn).Encode(req); err != nil {
				return err
			}
			var result map[string]any
			if err = json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&result); err != nil {
				return err
			}
			if err = json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
				return err
			}
			if result["status"] == "refused" {
				return errors.New("resident operation refused; consume original readback")
			}
			return nil
		}}
		if operation == "acquire" {
			child.Flags().StringVar(&specFile, "spec-file", "", "Explicit foreground network service contract")
			_ = child.MarkFlagRequired("spec-file")
		}
		if operation == "release" {
			child.Flags().StringVar(&id, "id", "", "Native resident identity from acquire")
			_ = child.MarkFlagRequired("id")
		}
		parent.AddCommand(child)
	}
	return parent
}
func (h *noodleHost) residentOperation(ctx context.Context, req hostRequest) any {
	result := map[string]any{"schema": 1, "owner": "noodle.swap-manager", "host_instance_id": h.instanceID, "status": "observed", "resources": h.budget.Snapshot(), "next": nil}
	refuse := func(err error) any { result["status"] = "refused"; result["error"] = err.Error(); return result }
	if h.residents == nil {
		return refuse(errors.New("resident management unavailable"))
	}
	if req.Operation == "resident_reconcile" {
		rows, err := h.residents.Reconcile(ctx)
		result["managed"] = rows
		if err != nil {
			return refuse(err)
		}
		return result
	}
	if req.Operation == "resident_inspect" {
		rows, err := h.residents.Snapshot()
		result["managed"] = rows
		if err != nil {
			return refuse(err)
		}
		processes, err := admission.ObserveResidents(ctx)
		result["processes"] = processes
		result["inventory_error"] = nil
		if err != nil {
			result["inventory_error"] = err.Error()
		}
		return result
	}
	project, err := filepath.Abs(req.ProjectDir)
	if err != nil {
		return refuse(err)
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return refuse(err)
	}
	var owner *hostContext
	for _, c := range h.contexts {
		if c.receipt.ProjectDir == project && c.receipt.ContextState == "active" && !c.failed {
			if owner != nil {
				return refuse(errors.New("resident context is ambiguous"))
			}
			owner = c
		}
	}
	if owner == nil {
		return refuse(errors.New("active native context missing for resident lease"))
	}
	var resident admission.Resident
	switch req.Operation {
	case "resident_acquire":
		if h.blocked {
			return refuse(errors.New("Host recovery pending"))
		}
		if req.ResidentSpec == nil {
			return refuse(errors.New("resident spec missing"))
		}
		resident, err = h.residents.Acquire(*req.ResidentSpec, owner.receipt.ContextID, h.budget)
	case "resident_release":
		resident, err = h.residents.Release(req.ResidentID, owner.receipt.ContextID)
	default:
		return refuse(errors.New("resident operation unknown"))
	}
	result["resident"] = resident
	result["resources"] = h.budget.Snapshot()
	result["context_id"] = owner.receipt.ContextID
	result["next"] = map[string]any{"owner": "noodle.swap-manager", "operation": "readback", "argv": []string{h.binaryPath, "host", "resources", "residents", "inspect"}}
	if err != nil {
		return refuse(err)
	}
	result["status"] = resident.State
	return result
}
