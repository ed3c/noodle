package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
)

func newHostResourcesCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "resources", Short: "Read shared Host worker admission and pressure", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
		if err != nil {
			return fmt.Errorf("host resources unavailable: %w", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err := json.NewEncoder(conn).Encode(hostRequest{Operation: "resources"}); err != nil {
			return err
		}
		var result json.RawMessage
		if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.AddCommand(newHostResidentsCmd(app))
	cmd.AddCommand(newHostWarningGrantCmd(app))
	return cmd
}

func (h *noodleHost) resources() any {
	if h.budget == nil {
		return hostError("unavailable", fmt.Errorf("host resource admission is not configured"))
	}
	var residents any
	if h.residents != nil {
		rows, err := h.residents.Snapshot()
		residents = map[string]any{"records": rows, "error": nil}
		if err != nil {
			residents = map[string]any{"records": rows, "error": err.Error()}
		}
	}
	shutdown := map[string]any{"requested": h.shutdown != nil}
	if h.shutdown != nil {
		shutdown["pending_finalizations"] = h.shutdown.pending
		shutdown["diagnostic"] = h.shutdown.diagnostic
	}
	return map[string]any{"shutdown": shutdown, "host_root": h.controlRoot(), "host_dir": h.dir, "gateway_warning_grants": h.budget.GatewayWarningGrants(), "safe_host_switch": hostCapabilities(runtime.GOOS)["safe_host_switch"], "rolling_host_version": h.rollingVersion(), "warning_grants": h.budget.WarningGrants(), "rolling_error": h.rollingError(), "binary_path": h.binaryPath, "binary_sha256": h.binarySHA, "residents": residents, "schema": 1, "protocol": hostProtocol, "host_pid": os.Getpid(), "host_instance_id": h.instanceID, "contexts": len(h.contexts), "web_url": h.webURL, "resources": h.budget.Snapshot()}
}

func (h *noodleHost) rollingVersion() int {
	if h.rolling != nil {
		return 1
	}
	return 0
}

func (h *noodleHost) rollingError() string {
	if h.rolling != nil {
		return h.rolling.failure
	}
	return ""
}
