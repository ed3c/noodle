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
	for _, operation := range []string{"inspect", "reconcile", "acquire", "release", "recover"} {
		var specFile, id, requestFile, manifestFile, consumerLease string
		var nativeConsumer bool
		child := &cobra.Command{Use: operation, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			req := hostRequest{Operation: "resident_" + operation, ProjectDir: app.projectDir, ResidentID: id, NativeConsumer: nativeConsumer, ConsumerLease: consumerLease}
			if operation == "recover" {
				data, err := os.ReadFile(manifestFile)
				if err != nil {
					return err
				}
				dec := json.NewDecoder(bytes.NewReader(data))
				dec.DisallowUnknownFields()
				var manifest admission.FixtureManifest
				if err = dec.Decode(&manifest); err != nil {
					return err
				}
				if err = dec.Decode(new(any)); err != io.EOF {
					return errors.New("fixture manifest contains trailing JSON")
				}
				req.FixtureManifest = &manifest
			}
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
			if requestFile != "" {
				original := callHost(cmd.Context(), hostRequest{Operation: "status", ProjectDir: app.projectDir, RequestFile: requestFile})
				if (original.Status != "ok" && !(operation == "inspect" && original.Status == "unknown_effect")) || (operation != "inspect" && original.ContextState != "active") || original.HostInstanceID == "" || original.ContextID == "" {
					_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"owner": "noodle.swap-manager", "status": "refused", "original": original, "error": "original Host request unavailable for resident operation"})
					return errors.New("original Host request unavailable for resident operation")
				}
				req.Operation += "_bound"
				req.ProjectDir = original.ProjectDir
				req.ContextID = original.ContextID
				req.InstanceID = original.HostInstanceID
				req.RequestID = original.RequestID
				req.ConfigSHA256 = original.ConfigSHA256
				req.Mode = original.Mode
				req.BinarySHA256 = original.BinarySHA256
				dir = original.HostDir
			}
			conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
			if err != nil {
				return fmt.Errorf("resident Host unavailable: %w", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
			unknown := func(err error) error {
				if operation == "recover" {
					readback := []string{os.Args[0], "--project-dir", req.ProjectDir, "host", "resources", "residents", "inspect", "--request-file", requestFile}
					if encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"owner": "noodle.swap-manager", "status": "unknown_effect", "error": err.Error(), "readback": readback, "retry_control": false}); encodeErr != nil {
						return errors.Join(err, encodeErr)
					}
				}
				return err
			}
			if err = json.NewEncoder(conn).Encode(req); err != nil {
				return unknown(err)
			}
			var result map[string]any
			if err = json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&result); err != nil {
				return unknown(err)
			}
			if requestFile != "" {
				result["readback"] = map[string]any{"owner": "noodle.swap-manager", "operation": "readback", "argv": []string{os.Args[0], "--project-dir", req.ProjectDir, "host", "resources", "residents", "inspect", "--request-file", requestFile}}
				if result["next"] != nil {
					result["next"] = result["readback"]
				}
			}
			if err = json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
				return err
			}
			if result["status"] == "refused" {
				return errors.New("resident operation refused; consume original readback")
			}
			return nil
		}}
		if operation == "acquire" || operation == "release" || operation == "inspect" || operation == "recover" || operation == "reconcile" {
			child.Flags().StringVar(&requestFile, "request-file", "", "Original admitted Host request")
			if operation != "inspect" && operation != "reconcile" {
				_ = child.MarkFlagRequired("request-file")
			}
		}
		if operation == "recover" {
			child.Flags().StringVar(&manifestFile, "manifest-file", "", "Frozen stateless test fixture evidence manifest")
			_ = child.MarkFlagRequired("manifest-file")
		}
		if operation == "acquire" || operation == "release" {
			child.Flags().BoolVar(&nativeConsumer, "native-consumer", false, "Require original native MCP caller proof")
		}
		if operation == "acquire" {
			child.Flags().StringVar(&specFile, "spec-file", "", "Explicit foreground network service contract")
			_ = child.MarkFlagRequired("spec-file")
		}
		if operation == "release" {
			child.Flags().StringVar(&id, "id", "", "Native resident identity from acquire")
			child.Flags().StringVar(&consumerLease, "consumer-lease", "", "Original native consumer lease from acquire")
			_ = child.MarkFlagRequired("id")
		}
		parent.AddCommand(child)
	}
	return parent
}
func (h *noodleHost) residentOperation(ctx context.Context, req hostRequest) any {
	result := map[string]any{"schema": 1, "owner": "noodle.swap-manager", "host_instance_id": h.instanceID, "host_pid": os.Getpid(), "status": "observed", "resources": h.budget.Snapshot(), "next": nil}
	result["sharing_policy"] = map[string]any{
		"backend": "original_noodle_host", "resident_adapters": []string{"stateless_fixtures"},
		"unverified_services": "keep_private", "io_lock": "service_owned; no generic proxy or global lock",
		"lifecycle_lock": "original Host actor, Residents mutex, single Host Budget",
		"consumer_end":   "explicit lease release or verified native process generation exit",
	}
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
	if req.Operation == "resident_inspect_bound" {
		found := false
		for _, receipt := range h.receipts {
			if req.ContextID != "" && req.RequestID != "" && req.InstanceID == h.instanceID &&
				req.BinarySHA256 == h.binarySHA && hostReceiptMatches(req, receipt) {
				found = true
				break
			}
		}
		if !found {
			return refuse(errors.New("original resident readback binding unavailable"))
		}
	}
	if req.Operation == "resident_inspect" || req.Operation == "resident_inspect_bound" {
		rows, err := h.residents.Snapshot()
		result["managed"] = rows
		fixtureRows, fixtureErr := h.residents.FixtureRecoveries().Snapshot()
		result["fixture_recoveries"] = fixtureRows
		if fixtureErr != nil {
			return refuse(fixtureErr)
		}
		if err != nil {
			return refuse(err)
		}
		processes, err := admission.ObserveVisibleProcesses(ctx)
		result["inventory_scope"] = "visible_all_users_metadata_only"
		result["processes"] = processes
		result["inventory_error"] = nil
		if err != nil {
			result["inventory_error"] = err.Error()
		}
		return result
	}
	if (req.Operation != "resident_acquire_bound" && req.Operation != "resident_release_bound" && req.Operation != "resident_recover_bound" && req.Operation != "resident_reconcile_bound") ||
		req.ContextID == "" || req.RequestID == "" || req.InstanceID != h.instanceID || req.BinarySHA256 != h.binarySHA {
		return refuse(errors.New("exact original Host request binding required for resident mutation"))
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
	if owner.receipt.ContextID != req.ContextID || owner.receipt.RequestID != req.RequestID {
		return refuse(errors.New("resident request no longer owns the active context"))
	}
	var consumer admission.ResidentConsumer
	if req.NativeConsumer {
		if !hostReceiptMatches(req, owner.receipt) {
			return refuse(errors.New("exact original consumer request receipt required"))
		}
		if req.peerError != nil || req.peerPID <= 1 {
			return refuse(errors.New("kernel native caller proof unavailable"))
		}
		home, e := os.UserHomeDir()
		if e != nil {
			return refuse(e)
		}
		registryRoot := os.Getenv("CODEX_HOME")
		if registryRoot == "" {
			registryRoot = filepath.Join(home, ".codex")
		}
		consumer, err = admission.ResolveNativeConsumer(ctx, filepath.Join(registryRoot, "hooks", "soodles-resources.json"), req.peerPID, req.RequestID, req.ContextID, project)
		if err != nil {
			return refuse(err)
		}
	}
	var resident admission.Resident
	switch req.Operation {
	case "resident_acquire_bound":
		if h.blocked {
			return refuse(errors.New("Host recovery pending"))
		}
		if req.ResidentSpec == nil {
			return refuse(errors.New("resident spec missing"))
		}
		if req.NativeConsumer {
			resident, err = h.residents.AcquireConsumer(*req.ResidentSpec, consumer, h.budget)
		} else {
			resident, err = h.residents.Acquire(*req.ResidentSpec, owner.receipt.ContextID, h.budget)
		}
	case "resident_release_bound":
		if req.NativeConsumer {
			resident, err = h.residents.ReleaseConsumer(req.ResidentID, consumer, req.ConsumerLease)
		} else {
			resident, err = h.residents.Release(req.ResidentID, owner.receipt.ContextID)
		}
	case "resident_recover_bound":
		if !hostReceiptMatches(req, owner.receipt) {
			return refuse(errors.New("exact original recovery request receipt required"))
		}
		if h.blocked {
			return refuse(errors.New("Host recovery pending"))
		}
		if req.FixtureManifest == nil {
			return refuse(errors.New("fixture evidence manifest missing"))
		}
		for _, candidate := range req.FixtureManifest.Candidates {
			if candidate.Origin != "historical" {
				return refuse(errors.New("future fixtures must register through their original launching owner"))
			}
		}
		if err = h.residents.FixtureRecoveries().Register(*req.FixtureManifest, owner.receipt.RequestID+"/"+owner.receipt.ContextID); err != nil {
			return refuse(err)
		}
		h.reconcileFixtureLifecycle(ctx)
		result["status"] = "accepted"
		result["readback_only_after_unknown"] = true
		result["next"] = map[string]any{"owner": "noodle.swap-manager", "operation": "readback", "argv": []string{h.binaryPath, "host", "resources", "residents", "inspect"}}
		return result
	case "resident_reconcile_bound":
		if !hostReceiptMatches(req, owner.receipt) {
			return refuse(errors.New("exact original recovery request receipt required"))
		}
		rows, reconcileErr := h.residents.Reconcile(ctx)
		if reconcileErr != nil {
			return refuse(reconcileErr)
		}
		result["managed"] = rows
		h.reconcileFixtureLifecycle(ctx)
		result["status"] = "accepted"
		return result
	default:
		return refuse(errors.New("resident operation unknown"))
	}
	result["resident"] = resident
	if req.NativeConsumer {
		for _, lease := range resident.ConsumerLeases {
			if lease.Connection == consumer.Connection && lease.SessionID == consumer.SessionID && lease.RequestID == consumer.RequestID {
				result["consumer_lease"] = lease.ID
				break
			}
		}
	}
	result["resources"] = h.budget.Snapshot()
	result["context_id"] = owner.receipt.ContextID
	result["next"] = map[string]any{"owner": "noodle.swap-manager", "operation": "readback", "argv": []string{h.binaryPath, "host", "resources", "residents", "inspect"}}
	if err != nil {
		return refuse(err)
	}
	result["status"] = resident.State
	return result
}
