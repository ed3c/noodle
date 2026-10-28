package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/spf13/cobra"
)

func newHostWarningGrantCmd(app *App) *cobra.Command {
	var requestFile, orderID string
	cmd := &cobra.Command{Use: "grant-warning", Short: "Allow one bound task launch under stable warning pressure for ten minutes", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			original := callHost(cmd.Context(), hostRequest{Operation: "status", ProjectDir: app.projectDir, RequestFile: requestFile})
			if original.Status != "ok" || original.ContextState != "active" || original.ContextID == "" || original.HostInstanceID == "" {
				return errors.New("original active Host request unavailable for warning grant")
			}
			req := hostRequest{Operation: "grant_warning", ProjectDir: original.ProjectDir, ContextID: original.ContextID,
				InstanceID: original.HostInstanceID, RequestID: original.RequestID, ConfigPath: original.ConfigPath,
				ConfigSHA256: original.ConfigSHA256, BinarySHA256: original.BinarySHA256, Mode: original.Mode,
				CleanupCompleted: original.CleanupCompleted, WarningOrderID: orderID}
			conn, err := net.DialTimeout("unix", filepath.Join(original.HostDir, "host.sock"), time.Second)
			if err != nil {
				return fmt.Errorf("warning grant Host unavailable: %w", err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			if err := json.NewEncoder(conn).Encode(req); err != nil {
				return err
			}
			var result map[string]any
			if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&result); err != nil {
				return err
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
				return err
			}
			switch result["status"] {
			case "available", "consumed", "expired", "revoked":
				return nil
			default:
				return errors.New("warning grant refused or unavailable; inspect original Host resources")
			}
		}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original admitted Host request")
	cmd.Flags().StringVar(&orderID, "order-id", "", "One explicitly admitted task order")
	_ = cmd.MarkFlagRequired("request-file")
	_ = cmd.MarkFlagRequired("order-id")
	return cmd
}

func (h *noodleHost) grantWarning(req hostRequest) any {
	result := map[string]any{"schema": 1, "host_instance_id": h.instanceID, "status": "refused"}
	refuse := func(err error) any { result["error"] = err.Error(); return result }
	if h.budget == nil || h.blocked {
		return refuse(errors.New("Host resource owner unavailable"))
	}
	if req.InstanceID != h.instanceID || req.BinarySHA256 != h.binarySHA || req.ContextID == "" || req.RequestID == "" {
		return refuse(errors.New("exact original Host request binding unavailable"))
	}
	owner := h.contexts[req.ContextID]
	if owner == nil || owner.failed || owner.receipt.ContextState != "active" || !hostReceiptMatches(req, owner.receipt) {
		return refuse(errors.New("warning grant request no longer owns the active context"))
	}
	data, err := readHostRequestConfig(req)
	if err != nil {
		return refuse(err)
	}
	if hostDigest(data) != req.ConfigSHA256 {
		return refuse(errors.New("warning grant configuration source changed"))
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return refuse(err)
	}
	if !slices.Contains(cfg.Runtime.AdmittedOrders, req.WarningOrderID) {
		return refuse(errors.New("warning grant order is outside the explicitly admitted task scope"))
	}
	grant, err := h.budget.GrantWarning(req.ContextID, req.WarningOrderID)
	if err != nil {
		return refuse(err)
	}
	result["status"], result["grant"] = grant.Status, grant
	result["resources"] = h.budget.Snapshot()
	return result
}
