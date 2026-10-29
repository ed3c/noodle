package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/loop"
	"github.com/poteto/noodle/worktree"
	"github.com/spf13/cobra"
)

func newHostCandidateReadbackCmd() *cobra.Command {
	var requestFile, orderID string
	cmd := &cobra.Command{Use: "candidate-readback", Short: "Read original task worktree custody and literal scope files", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		raw, err := os.ReadFile(requestFile)
		if err != nil {
			return err
		}
		var binding hostRequestBinding
		if err = json.Unmarshal(raw, &binding); err != nil {
			return err
		}
		if binding.Schema != 1 || binding.Status != "accepted" || binding.Operation != "attach" {
			return fmt.Errorf("original accepted task request unavailable")
		}
		p, err := readHostPool(binding.HostDir)
		if err != nil {
			return err
		}
		owner, ok := p.Owners[binding.RequestID]
		req := hostRequest{ProjectDir: binding.ProjectDir, ConfigPath: binding.ConfigPath, ConfigSHA256: binding.ConfigSHA256, Mode: binding.Mode, RequestID: binding.RequestID, CleanupCompleted: binding.CleanupCompleted}
		if !ok || !hostReceiptMatches(req, owner.Original) || binding.BinarySHA256 != owner.Original.BinarySHA256 || binding.EnvironmentSHA256 != owner.EnvironmentSHA {
			return fmt.Errorf("original task request binding changed")
		}
		data, err := readHostRequestConfig(req)
		if err != nil || hostDigest(data) != binding.ConfigSHA256 {
			return fmt.Errorf("original config source unavailable or changed")
		}
		cfg, err := config.Parse(data)
		if err != nil {
			return err
		}
		admitted := cfg.Runtime.AdmittedOrders
		legacy := cfg.Runtime.Process.AdmittedOrders
		if cfg.Runtime.Default == "process" && len(admitted) == 0 {
			admitted = legacy
		}
		if !slices.Contains(admitted, orderID) ||
			(cfg.Runtime.Default == "process" && len(legacy) != 0 && !slices.Contains(legacy, orderID)) {
			return fmt.Errorf("order is outside original admitted task")
		}
		snapshot, err := reducer.ReadSnapshot(filepath.Join(binding.ProjectDir, ".noodle", "state.snapshot.json"))
		if err != nil {
			return err
		}
		order, ok := snapshot.State.Orders[orderID]
		if !ok || len(order.Stages) != 1 {
			return fmt.Errorf("original task stage unavailable")
		}
		stage := order.Stages[0]
		if len(stage.Attempts) == 0 {
			return fmt.Errorf("original task attempt unavailable")
		}
		attempt := stage.Attempts[len(stage.Attempts)-1]
		var prompt struct {
			Kind       string   `json:"kind"`
			WritePaths []string `json:"write_paths"`
		}
		if json.Unmarshal([]byte(stage.Prompt), &prompt) != nil || prompt.Kind != "local_task" || len(prompt.WritePaths) == 0 {
			return fmt.Errorf("original local task scope unavailable")
		}
		if attempt.WorktreeName == "" || attempt.WorktreeName == "." || attempt.WorktreeName == ".." || strings.ContainsAny(attempt.WorktreeName, "/\\") {
			return fmt.Errorf("original worktree name is invalid")
		}
		logical := filepath.Join(binding.ProjectDir, ".worktrees", attempt.WorktreeName)
		e, err := execution.New(binding.ProjectDir, os.Environ())
		if err != nil {
			return err
		}
		if err = validateHostEnvironment(e); err != nil {
			return err
		}
		custody, err := worktree.ReadCustody(cmd.Context(), e, logical)
		if err != nil {
			return err
		}
		files, err := loop.LocalTaskScopeFiles(e, custody.Physical, prompt.WritePaths)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema": 1, "request_id": binding.RequestID, "order_id": orderID, "session_id": attempt.SessionID, "custody": custody, "write_paths": prompt.WritePaths, "scope_files": names, "effects": []string{}})
	}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original accepted task request")
	cmd.Flags().StringVar(&orderID, "order-id", "", "Original admitted task order")
	_ = cmd.MarkFlagRequired("request-file")
	_ = cmd.MarkFlagRequired("order-id")
	return cmd
}
