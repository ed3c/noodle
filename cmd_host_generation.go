package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/spf13/cobra"
)

// A generation shares admission accounting; it never rebuilds retained work or
// consumes the historical version queue. Those records are custody data only.
type hostRolling struct {
	root, generation string
}

func (h *noodleHost) rollingVersion() int {
	if h.rolling != nil {
		return 1
	}
	return 0
}

func hostCapabilities() map[string]any {
	return map[string]any{"schema": 1, "protocol": hostProtocol, "resident_request_binding": 1, "native_consumer_leases": 1, "native_consumer_worktrees": 1, "stateless_fixture_recovery": 1, "shared_service_adapter": "stateless_fixtures_only", "restricted_host_maintenance": 1, "rolling_host_version": 1, "shared_worker_quota": 1, "worker_admission_policy": admission.WorkerAdmissionPolicy}
}

func newHostBudget() (*admission.Budget, error) {
	limit := admission.DefaultSwapUsedLimit
	if raw, ok := os.LookupEnv("NOODLE_HOST_SWAP_USED_LIMIT_BYTES"); ok {
		value, err := strconv.ParseUint(raw, 10, 53)
		if err != nil || raw == "" || strings.TrimSpace(raw) != raw {
			return nil, errors.New("NOODLE_HOST_SWAP_USED_LIMIT_BYTES must be an integer in 0..9007199254740991")
		}
		limit = value
	}
	return admission.NewBudgetWithSwapUsedLimit(0, limit), nil
}

func readHostRequestConfig(req hostRequest) ([]byte, error) {
	if req.ConfigPath == "" {
		return readHostConfig(req.ProjectDir)
	}
	if !filepath.IsAbs(req.ConfigPath) {
		return nil, errors.New("task config source must be absolute")
	}
	return os.ReadFile(req.ConfigPath)
}

func hostGenerationFence(dir string) (*lockfile.Lock, error) {
	root, generation := os.Getenv("NOODLE_HOST_POOL_ROOT"), os.Getenv("NOODLE_HOST_GENERATION")
	if root == "" && generation == "" {
		return lockfile.TryLock(filepath.Join(dir, "host.lock"))
	}
	if !filepath.IsAbs(root) || !validPoolID(generation) || dir != filepath.Join(root, "g", generation) {
		return nil, errors.New("generation scope is invalid")
	}
	return lockfile.TryLockShared(filepath.Join(root, "host.lock"))
}

func joinHostGeneration(dir, id, binary, digest string, budget *admission.Budget) (*hostRolling, error) {
	root, generation := os.Getenv("NOODLE_HOST_POOL_ROOT"), os.Getenv("NOODLE_HOST_GENERATION")
	if root == "" && generation == "" {
		return nil, nil
	}
	if !filepath.IsAbs(root) || !validPoolID(generation) || id != generation || dir != filepath.Join(root, "g", generation) {
		return nil, errors.New("native generation scope differs")
	}
	p, err := readHostPool(root)
	if err != nil {
		return nil, err
	}
	g, ok := p.Generations[id]
	if !ok || g.Binary != binary || g.SHA != digest {
		return nil, errors.New("selected generation executable differs")
	}
	if err = budget.BindSharedQuota(filepath.Join(root, "quota"), id); err != nil {
		return nil, err
	}
	return &hostRolling{root: root, generation: id}, nil
}

func (h *noodleHost) forwardGeneration(ctx context.Context, req hostRequest) (any, bool) {
	if h.rolling == nil {
		return nil, false
	}
	p, err := readHostPool(h.rolling.root)
	if err != nil {
		return hostError("unknown_effect", err), true
	}
	owner, owned := p.Owners[req.RequestID]
	if !owned && req.RequestID == "" && req.ContextID != "" {
		for _, candidate := range p.Owners {
			if candidate.Current.ContextID == req.ContextID && candidate.Current.HostInstanceID == req.InstanceID {
				owner, owned = candidate, true
				break
			}
		}
	}
	if owned {
		if !hostReceiptMatches(req, owner.Original) {
			return hostError("refused", errors.New("original task binding differs")), true
		}
		if owner.MaintenanceHold != "" && req.Operation != "status" {
			return hostResponse{owner.Current, "busy", "original work is held by restricted maintenance; no dispatch or rebuild allowed"}, true
		}
	}
	g, err := poolRequestOwner(h.rolling.root, req)
	if err != nil {
		return hostError("refused", err), true
	}
	if g.ID == h.instanceID {
		return nil, false
	}
	if g.State == "exited" {
		if req.Operation == "status" {
			req.BinarySHA256 = g.SHA
			return offlineHostStatus(g.Directory, req, errors.New("original generation exited")), true
		}
		return hostError("refused", errors.New("original generation ended; no replay attempted")), true
	}
	req.BinarySHA256 = g.SHA
	raw, err := nativeHostRPC(ctx, g.Directory, req)
	if err != nil {
		return hostError("unknown_effect", err), true
	}
	return raw, true
}

func (h *noodleHost) recordGenerationReceipt(req hostRequest, r hostReceipt) error {
	if h.rolling == nil {
		return nil
	}
	return updateHostPool(h.rolling.root, func(p *hostPool) error {
		owner, exists := p.Owners[req.RequestID]
		if exists && owner.MaintenanceHold != "" {
			return errors.New("retained request is held")
		}
		if !exists {
			original := r
			if req.AdmissionSHA != "" {
				original.BinarySHA256 = req.AdmissionSHA
				original.BinaryPath = req.AdmissionPath
			}
			owner.Original = original
			owner.EnvironmentSHA = hostDigest([]byte(strings.Join(req.Environment, "\x00")))
		}
		owner.Current = r
		owner.State = "owned"
		if r.ContextState == "detached" {
			owner.State = "terminal"
		}
		if r.ContextState == "failed" {
			owner.State = "unknown"
		}
		p.Owners[req.RequestID] = owner
		return nil
	})
}

func newHostRollingCmd() *cobra.Command {
	var binary string
	var plan, inspect bool
	var requests []string
	cmd := &cobra.Command{Use: "switch", Short: "Read a source-bound restricted maintenance plan", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		root, err := existingHostDirectory()
		if err != nil {
			return err
		}
		if inspect {
			if plan {
				return errors.New("status and maintenance-plan are separate readbacks")
			}
			p, err := readHostPool(root)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		if !plan {
			return errors.New("use --maintenance-plan; activation requires host maintenance with an exact confirmed digest")
		}
		if !filepath.IsAbs(binary) {
			return errors.New("target binary must be absolute")
		}
		raw, err := os.ReadFile(binary)
		if err != nil {
			return err
		}
		p, err := planHostMaintenance(cmd.Context(), root, binary, hostDigest(raw), requests)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
	}}
	cmd.Flags().StringVar(&binary, "binary", "", "Immutable target executable")
	cmd.Flags().BoolVar(&plan, "maintenance-plan", false, "Read the original owner and migration preconditions without effects")
	cmd.Flags().BoolVar(&inspect, "status", false, "Read existing generation records only")
	cmd.Flags().StringArrayVar(&requests, "request-file", nil, "Original accepted request envelope")
	return cmd
}

func verifyRollingTarget(ctx context.Context, binary, digest string) error {
	info, err := os.Lstat(binary)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return errors.New("target is not a regular executable")
	}
	raw, err := os.ReadFile(binary)
	if err != nil || hostDigest(raw) != digest {
		return errors.New("target executable changed")
	}
	raw, err = exec.CommandContext(ctx, binary, "host", "capabilities").Output()
	if err != nil {
		return err
	}
	var c struct {
		Protocol    string `json:"protocol"`
		Pool        int    `json:"rolling_host_version"`
		Quota       int    `json:"shared_worker_quota"`
		Maintenance int    `json:"restricted_host_maintenance"`
		Policy      string `json:"worker_admission_policy"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Protocol != hostProtocol || c.Pool != 1 || c.Quota != 1 || c.Maintenance != 1 || c.Policy != admission.WorkerAdmissionPolicy {
		return errors.New("target lacks restricted maintenance and shared quota contracts")
	}
	return nil
}
