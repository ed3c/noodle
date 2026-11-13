package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

// This snapshot prepares the original owner's policy change. It does not fence
// admission, retire a process, revise custody, or authorize those effects.
type hostOperatorPlan struct {
	Schema         int                                        `json:"schema"`
	Request        loopruntime.SessionReadbackFileRef         `json:"request"`
	Config         loopruntime.SessionReadbackFileRef         `json:"config"`
	Profile        loopruntime.SessionReadbackFileRef         `json:"profile"`
	ProfileSource  string                                     `json:"profile_source"`
	History        loopruntime.SessionReadbackFileRef         `json:"context_history"`
	Pool           loopruntime.SessionReadbackFileRef         `json:"pool"`
	Custody        loopruntime.SessionReadbackFileRef         `json:"custody"`
	Source         json.RawMessage                            `json:"native_source"`
	Contexts       []hostReceipt                              `json:"contexts"`
	Binding        loopruntime.OpenShellCustodyBinding        `json:"original_binding"`
	Resident       admission.Resident                         `json:"original_resident"`
	NativeScope    *hostOperatorNativeScope                   `json:"native_scope,omitempty"`
	OperatorPolicy *loopruntime.SourceOperatorPolicyCandidate `json:"operator_policy_candidate,omitempty"`
	OperatorArgv   []string                                   `json:"proposed_resident_argv"`
	Requires       []string                                   `json:"requires"`
	Executable     bool                                       `json:"executable"`
	Effects        []string                                   `json:"effects"`
}

func newHostOperatorPlanCmd() *cobra.Command {
	var request string
	var native bool
	var holdSnapshot string
	var profileOutput string
	cmd := &cobra.Command{Use: "operator-plan", Short: "Read the original Source request and prepare its gateway operator policy; no service effects", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if holdSnapshot != "" {
				if request != "" || native || profileOutput != "" {
					return errors.New("hold-plan-file cannot be combined with request-file, native-scope or operator-profile-output")
				}
				plan, err := prepareSourcePolicyHold(holdSnapshot)
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
			}
			plan, err := planHostOperatorMaintenance(cmd.Context(), request)
			if err != nil {
				return err
			}
			if native {
				plan.NativeScope, err = observeHostOperatorNativeScope(cmd.Context(), plan)
				if err != nil {
					return err
				}
			}
			if profileOutput != "" {
				candidate, err := loopruntime.PrepareSourceOperatorPolicy(plan.Profile, plan.ProfileSource, profileOutput)
				if err != nil {
					return err
				}
				if candidate.BeforeBinding != plan.Binding {
					return errors.New("Source operator candidate differs from original live custody binding")
				}
				plan.OperatorPolicy = &candidate
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
		}}
	cmd.Flags().StringVar(&request, "request-file", "", "Original accepted Source attach request")
	cmd.Flags().BoolVar(&native, "native-scope", false, "Bind original Host/gateway/VM-driver kernel generations and declared namespace; no control or zero-compute claim")
	cmd.Flags().StringVar(&holdSnapshot, "hold-plan-file", "", "Prepare only the original maintenance hold from a saved native-scope snapshot; no service effects")
	cmd.Flags().StringVar(&profileOutput, "operator-profile-output", "", "Materialize only an auth-only Source profile candidate; original profile and service remain unchanged")
	return cmd
}

func planHostOperatorMaintenance(ctx context.Context, path string) (hostOperatorPlan, error) {
	p := hostOperatorPlan{Schema: 1, Effects: []string{}, Requires: []string{
		"Original-owner admission fence covering the live generation and its direct entry",
		"Fresh live resident identity and original custody proof validation under that fence",
		"Fresh deployment-bound compute evidence under that fence before generation retirement",
		"Preserve original contexts and unknown outcomes as held after confirmed retirement",
		"Source policy revision linking the original custody proof to the new binding",
	}}
	var request hostRequestBinding
	var err error
	p.Request, err = loopruntime.ReadSessionReadbackFile(path, &request)
	if err != nil {
		return p, err
	}
	if request.Schema != 1 || request.Status != "accepted" || request.Operation != "attach" || !validPoolID(request.RequestID) || !filepath.IsAbs(request.HostDir) {
		return p, errors.New("operator plan requires the original accepted Source attach request")
	}
	p.Source, err = nativeHostRPC(ctx, request.HostDir, hostRequest{Operation: "resources"})
	if err != nil {
		return p, err
	}
	var source struct {
		legacyHostResources
		Root      string          `json:"host_root"`
		Directory string          `json:"host_dir"`
		BinarySHA string          `json:"binary_sha256"`
		Resources json.RawMessage `json:"resources"`
		Residents *struct {
			Records []admission.Resident `json:"records"`
			Error   any                  `json:"error"`
		} `json:"residents"`
	}
	if err = json.Unmarshal(p.Source, &source); err != nil {
		return p, err
	}
	if source.Protocol != hostProtocol || source.Rolling != 1 || !validPoolID(source.Instance) || source.PID <= 0 || source.Root != request.HostDir || source.Directory != filepath.Join(request.HostDir, "g", source.Instance) || source.BinarySHA != request.BinarySHA256 || len(source.Resources) == 0 || string(source.Resources) == "null" {
		return p, errors.New("original rolling Source resource identity differs")
	}
	var receipts []hostReceipt
	p.History, err = loopruntime.ReadSessionReadbackFile(filepath.Join(source.Directory, "receipts.json"), &receipts)
	if err != nil {
		return p, err
	}
	var pool hostPool
	p.Pool, err = loopruntime.ReadSessionReadbackFile(filepath.Join(request.HostDir, "pool.json"), &pool)
	if err != nil {
		return p, err
	}
	generation, exists := pool.Generations[source.Instance]
	if pool.Schema != 1 || !exists || pool.Active != source.Instance || generation.ID != source.Instance || generation.Directory != source.Directory {
		return p, errors.New("original Source pool generation differs")
	}
	seen := map[string]bool{}
	var selected *hostReceipt
	for _, receipt := range receipts {
		if receipt.ContextState == "detached" {
			continue
		}
		owner, exists := pool.Owners[receipt.RequestID]
		identity := hostRequest{ProjectDir: receipt.ProjectDir, ConfigPath: receipt.ConfigPath, ConfigSHA256: receipt.ConfigSHA256, Mode: receipt.Mode, RequestID: receipt.RequestID, ContextID: receipt.ContextID, InstanceID: receipt.HostInstanceID, CleanupCompleted: receipt.CleanupCompleted}
		// Lifecycle state can differ between the original history and pool after
		// a failed finalization. Preserve both files; compare identity, not outcome.
		if !exists || !hostReceiptMatches(identity, owner.Current) || owner.Current.HostPID != receipt.HostPID || owner.Current.BinaryPath != receipt.BinaryPath || owner.Current.BinarySHA256 != receipt.BinarySHA256 || receipt.BinarySHA256 != source.BinarySHA || receipt.HostInstanceID != source.Instance || receipt.HostPID != source.PID || !validPoolID(receipt.ContextID) || seen[receipt.ContextID] {
			return p, errors.New("original Source context custody differs")
		}
		seen[receipt.ContextID] = true
		p.Contexts = append(p.Contexts, receipt)
		if receipt.RequestID == request.RequestID {
			copy := receipt
			selected = &copy
		}
	}
	if selected == nil || len(p.Contexts) != source.Contexts {
		return p, errors.New("original Source context set is incomplete")
	}
	inputs := planHostMaintenanceContextInputs(request.HostDir, *selected, []string{p.Request.Path})
	if len(inputs.Blockers) != 0 {
		return p, fmt.Errorf("original Source inputs unavailable: %s", strings.Join(inputs.Blockers, "; "))
	}
	p.Config, err = loopruntime.ReadSessionReadbackFile(inputs.ConfigSource, nil)
	if err != nil {
		return p, err
	}
	data, err := os.ReadFile(p.Config.Path)
	if err != nil || hostDigest(data) != p.Config.SHA256 || p.Config.SHA256 != selected.ConfigSHA256 {
		return p, errors.New("original Source config changed during preparation")
	}
	cfg, err := config.Parse(data)
	if err != nil || !filepath.IsAbs(cfg.Runtime.OpenShell.Profile) {
		return p, errors.New("original Source gateway profile unavailable")
	}
	p.Profile, err = loopruntime.ReadSessionReadbackFile(cfg.Runtime.OpenShell.Profile, nil)
	if err != nil {
		return p, err
	}
	p.ProfileSource = cfg.Runtime.OpenShell.Profile
	gateway, err := loopruntime.LoadOpenShellGatewaySpec(cfg.Runtime.OpenShell.Profile)
	if err != nil {
		return p, err
	}
	for _, arg := range gateway.Resident.Argv {
		if arg == "--enable-operator-auth" || strings.HasPrefix(arg, "--enable-operator-auth=") {
			return p, errors.New("Source operator policy is already explicit; inspect it before proposing a revision")
		}
	}
	var custody loopruntime.OpenShellCustodyState
	p.Custody, err = loopruntime.ReadSessionReadbackFile(filepath.Join(gateway.Binding.StateRoot, "owner.json"), &custody)
	if err != nil {
		return p, err
	}
	if (custody.Version != 4 && custody.Version != 5) || custody.Binding != gateway.Binding || custody.ControlRoot != request.HostDir {
		return p, errors.New("Source profile differs from original custody deployment or Host control root")
	}
	guardID, err := loopruntime.OpenShellGatewayGuardID(gateway)
	if err != nil {
		return p, err
	}
	if source.Residents == nil || source.Residents.Error != nil || len(source.Residents.Records) != 1 {
		return p, errors.New("original Source must expose exactly one observable gateway resident")
	}
	resident := source.Residents.Records[0]
	if resident.ID != gateway.Resident.Name || resident.Name != gateway.Resident.Name || resident.Sharing != "shared" || resident.State != "running" || resident.PID <= 1 || resident.ProcessGroup != resident.PID || resident.Started == "" || resident.GuardID != guardID || len(resident.ConsumerLeases) != 0 {
		return p, errors.New("original live Source resident differs from its gateway deployment")
	}
	selectedLease := false
	for _, id := range resident.Contexts {
		if !seen[id] {
			return p, errors.New("Source gateway serves a context outside its original Host set")
		}
		selectedLease = selectedLease || id == selected.ContextID
	}
	if !selectedLease {
		return p, errors.New("original Source request does not hold its live gateway resident")
	}
	p.Resident = resident
	p.Binding = gateway.Binding
	p.OperatorArgv = append(append([]string{}, gateway.Resident.Argv...), "--enable-operator-auth", "true")
	for _, ref := range []loopruntime.SessionReadbackFileRef{p.Request, p.Config, p.Profile, p.History, p.Pool, p.Custody} {
		current, readErr := loopruntime.ReadSessionReadbackFile(ref.Path, nil)
		if readErr != nil || current != ref {
			return p, errors.New("original Source inputs changed during operator preparation")
		}
	}
	return p, nil
}
