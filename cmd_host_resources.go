package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
	"sort"
)

func newHostResourcesCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "resources", Short: "Read shared Host worker admission and pressure", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := hostReadDirectory()
		if err != nil {
			return err
		}
		conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
		if err != nil {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(readRetainedResidents(dir, err))
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err := json.NewEncoder(conn).Encode(hostRequest{Operation: "resources"}); err != nil {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(readRetainedResidents(dir, err))
		}
		var result json.RawMessage
		if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&result); err != nil {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(readRetainedResidents(dir, err))
		}
		var status *struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(result, &status); err != nil {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(readRetainedResidents(dir, err))
		}
		if status == nil {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(readRetainedResidents(dir, fmt.Errorf("Host resource response is null")))
		}
		if status.Status == "refused" || status.Status == "unavailable" || status.Status == "binary_mismatch" {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(readRetainedResidents(dir, fmt.Errorf("Host resource readback %s: %s", status.Status, status.Error)))
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.AddCommand(newHostResidentsCmd(app))
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
	return map[string]any{"residents": residents, "schema": 1, "protocol": hostProtocol, "host_pid": os.Getpid(), "host_instance_id": h.instanceID, "contexts": len(h.contexts), "web_url": h.webURL, "resources": h.budget.Snapshot()}
}

// Offline resource readback preserves stored process facts without opening a Host.
func readRetainedResidents(dir string, unavailable error) any {
	path := filepath.Join(dir, "residents.json")
	gaps := []loop.InspectionGap{{Path: filepath.Join(dir, "host.sock"), Code: "live_unavailable", Detail: unavailable.Error()}}
	managed := []admission.Resident{}
	data, err := os.ReadFile(path)
	if err != nil {
		gaps = append(gaps, loop.InspectionGap{Path: path, Code: "unavailable", Detail: err.Error()})
	} else {
		var records map[string]*admission.Resident
		if err = json.Unmarshal(data, &records); err != nil {
			gaps = append(gaps, loop.InspectionGap{Path: path, Code: "invalid", Detail: err.Error()})
		} else {
			ids := make([]string, 0, len(records))
			for id := range records {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				if records[id] == nil {
					gaps = append(gaps, loop.InspectionGap{Path: path, Code: "invalid", Detail: "null resident record " + id})
					continue
				}
				managed = append(managed, *records[id])
			}
		}
	}
	return map[string]any{"schema": 1, "status": "observed", "observation_scope": "persisted_only", "live_verified": false, "managed": managed, "gaps": gaps, "next": nil}
}
