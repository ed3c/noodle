package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/state"
	"github.com/spf13/cobra"
)

// Recovery never adopts a worker. Only a failed context with no dispatch history
// can be detached while both the Host and project locks are held here.
func newHostRecoverCmd(app *App) *cobra.Command {
	var requestFile string
	cmd := &cobra.Command{Use: "recover-unstarted", Short: "Detach an offline failed context only when native records prove no worker started", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := recoverUnstarted(app.projectDir, requestFile)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original native request binding (unchanged)")
	_ = cmd.MarkFlagRequired("request-file")
	return cmd
}

func recoverUnstarted(project, requestFile string) (hostResponse, error) {
	fail := func(err error) (hostResponse, error) { return hostResponse{}, err }
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		return fail(err)
	}
	data, err := os.ReadFile(requestFile)
	if err != nil {
		return fail(err)
	}
	var binding hostRequestBinding
	if err = json.Unmarshal(data, &binding); err != nil {
		return fail(err)
	}
	if binding.Schema != 1 || binding.Operation != "attach" || binding.RequestID == "" || binding.ProjectDir != project || !filepath.IsAbs(binding.HostDir) {
		return fail(errors.New("original attach binding invalid"))
	}
	hostLock, err := lockfile.TryLock(filepath.Join(binding.HostDir, "host.lock"))
	if err != nil {
		return fail(fmt.Errorf("Host must be stopped: %w", err))
	}
	defer hostLock.Close()
	runtimeDir := filepath.Join(project, ".noodle")
	projectLock, err := lockfile.TryLock(filepath.Join(runtimeDir, "noodle.lock"))
	if err != nil {
		return fail(err)
	}
	defer projectLock.Close()
	data, err = os.ReadFile(filepath.Join(binding.HostDir, "receipts.json"))
	if err != nil {
		return fail(err)
	}
	var receipts []hostReceipt
	if err = json.Unmarshal(data, &receipts); err != nil {
		return fail(err)
	}
	index := -1
	for i, r := range receipts {
		if r.RequestID == binding.RequestID {
			if index != -1 {
				return fail(errors.New("ambiguous original request"))
			}
			index = i
		}
	}
	if index < 0 {
		return fail(errors.New("original receipt missing"))
	}
	original := receipts[index]
	if original.ProjectDir != project || original.ConfigSHA256 != binding.ConfigSHA256 || original.ConfigPath != binding.ConfigPath || original.BinarySHA256 != binding.BinarySHA256 || original.Mode != binding.Mode || original.HostDir != binding.HostDir {
		return fail(errors.New("original request/receipt binding differs"))
	}
	if original.ContextState == "detached" {
		return hostResponse{original, "ok", ""}, nil
	}
	if original.ContextState != "failed" {
		return fail(errors.New("only an explicitly failed context can recover"))
	}
	if original.HostPID <= 0 || procx.IsPIDAlive(original.HostPID) {
		return fail(errors.New("original Host process is still alive or identity is unknown"))
	}
	cfg, err := readHostRequestConfig(hostRequest{ProjectDir: project, ConfigPath: binding.ConfigPath})
	if err != nil {
		return fail(err)
	}
	if hostDigest(cfg) != binding.ConfigSHA256 {
		return fail(errors.New("original config drifted"))
	}
	evidence, err := unstartedEvidence(runtimeDir)
	if err != nil {
		return fail(err)
	}
	// Save original failure and native observations before the deterministic state transition.
	proof := struct {
		Original hostReceipt       `json:"original"`
		Evidence map[string]string `json:"evidence"`
	}{original, evidence}
	proofBytes, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		return fail(err)
	}
	proofPath := filepath.Join(binding.HostDir, "unstarted-"+original.ContextID+".json")
	if existing, err := os.ReadFile(proofPath); err == nil {
		if string(existing) != string(proofBytes) {
			return fail(errors.New("recovery evidence changed; original owner readback required"))
		}
	} else if !os.IsNotExist(err) {
		return fail(err)
	} else if err = filex.WriteFileAtomicDurable(proofPath, proofBytes); err != nil {
		return fail(err)
	}
	receipts[index].ContextState = "detached"
	data, err = json.MarshalIndent(receipts, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err = filex.WriteFileAtomicDurable(filepath.Join(binding.HostDir, "receipts.json"), data); err != nil {
		return fail(err)
	}
	return hostResponse{receipts[index], "ok", ""}, nil
}

func unstartedEvidence(dir string) (map[string]string, error) {
	evidence := map[string]string{}
	for _, name := range []string{"sessions", "../.worktrees"} {
		entries, err := os.ReadDir(filepath.Join(dir, name))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if len(entries) != 0 {
			return nil, fmt.Errorf("%s is nonempty; cannot prove unstarted", name)
		}
		evidence[name] = "absent_or_empty"
	}
	for _, name := range []string{"control.ndjson", "orders-next.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if len(data) != 0 {
			return nil, fmt.Errorf("%s is pending", name)
		}
		evidence[name] = hostDigest(data)
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		return nil, err
	}
	var snapshot struct {
		State  state.State       `json:"state"`
		Ledger []json.RawMessage `json:"effect_ledger"`
	}
	if err = json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.State.SchemaVersion != 1 || snapshot.State.Orders == nil || len(snapshot.State.PendingReviews) != 0 {
		return nil, errors.New("native snapshot has missing state or effect/review history")
	}
	for _, raw := range snapshot.Ledger {
		var entry struct {
			ID     string `json:"effect_id"`
			Status string `json:"status"`
			Effect struct {
				ID      string `json:"effect_id"`
				Type    string `json:"type"`
				Payload struct {
					OrderID string `json:"order_id"`
				} `json:"payload"`
			} `json:"effect"`
			Result struct {
				ID     string `json:"effect_id"`
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, err
		}
		if entry.Effect.Type != "initial_admission" || entry.Status != "done" || entry.Result.Status != "completed" || entry.Result.Error != "" || entry.ID == "" || entry.ID != entry.Effect.ID || entry.ID != entry.Result.ID || snapshot.State.Orders[entry.Effect.Payload.OrderID].OrderID != entry.Effect.Payload.OrderID || entry.Effect.Payload.OrderID == "" {
			return nil, errors.New("native effect is not a completed initial admission")
		}
	}
	for _, order := range snapshot.State.Orders {
		for _, stage := range order.Stages {
			if stage.Status != state.StagePending || len(stage.Attempts) != 0 || stage.Merge != nil {
				return nil, errors.New("native stage has dispatch/effect history")
			}
		}
	}
	evidence["state.snapshot.json"] = hostDigest(data)
	for _, name := range []string{"pending-review.json", "tickets.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if name == "pending-review.json" {
			var review struct {
				Items []json.RawMessage `json:"items"`
			}
			err = json.Unmarshal(data, &review)
			items = review.Items
		} else {
			err = json.Unmarshal(data, &items)
		}
		if err != nil {
			return nil, err
		}
		if len(items) != 0 {
			return nil, fmt.Errorf("%s has unresolved work", name)
		}
		evidence[name] = hostDigest(data)
	}
	return evidence, nil
}
