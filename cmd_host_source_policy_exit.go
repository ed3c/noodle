package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
)

// This is an observation in the original maintenance status, not a journal
// transition, credential-drain receipt or authority to retire/rebind anything.
type sourcePolicyNormalExit struct {
	State      string                  `json:"state"`
	Method     string                  `json:"method,omitempty"`
	Diagnostic string                  `json:"diagnostic,omitempty"`
	KernelExit *procx.NativeExitStatus `json:"kernel_exit,omitempty"`
	Parent     *procx.NativeIdentity   `json:"parent_kernel,omitempty"`
}

func readSourcePolicyStatus(ctx context.Context, p hostMaintenancePlan) (json.RawMessage, sourcePolicyNormalExit, error) {
	observation := sourcePolicyNormalExit{State: "unknown"}
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		return nil, observation, err
	}
	if op.Resident.PID != op.NativeScope.Gateway.Process.PID {
		return nil, observation, errors.New("normal-exit resident differs from the original gateway actor")
	}
	checkParent := func() (procx.NativeIdentity, error) {
		if p.Native == nil || !p.Native.SameGeneration(op.NativeScope.Host.Process) {
			return procx.NativeIdentity{}, errors.New("normal-exit observation lacks its original Host generation")
		}
		current, err := procx.ObserveNativeIdentity(p.Native.PID)
		if err != nil || !p.Native.SameGeneration(current) || current.Zombie {
			return procx.NativeIdentity{}, errors.New("normal-exit observation lost its original live parent")
		}
		return current, nil
	}
	parent, err := checkParent()
	if err != nil {
		return nil, observation, err
	}
	if parent.Stopped {
		// Do not CONT the whole old Host merely to run its waiter. The kernel
		// retains the child's wait status and exec version until that parent reaps.
		observation.Method = "kernel-zombie"
		exit, err := procx.ObserveNativeExitStatus(op.NativeScope.Gateway.Process)
		if err != nil {
			return nil, observation, err
		}
		current, err := checkParent()
		if err != nil || !current.Stopped || exit.ParentPID != current.PID {
			return nil, observation, errors.New("kernel exit lost its original stopped parent")
		}
		observation.KernelExit, observation.Parent = &exit, &current
		observation.State = "not_observed"
		if exit.NormalZero() {
			observation.State = "observed"
		} else {
			observation.Diagnostic = "original kernel wait status is not normal zero exit"
		}
		return nil, observation, nil
	}
	observation.Method = "original-parent-waiter"
	raw, err := nativeHostRPC(ctx, filepath.Join(p.Root, "g", p.Source.Instance), hostRequest{Operation: "resources"})
	if err != nil {
		return raw, observation, err
	}
	currentParent, err := checkParent()
	if err != nil || currentParent.Stopped {
		if err == nil {
			err = errors.New("original parent stopped during resource readback")
		}
		return raw, observation, err
	}
	var current struct {
		legacyHostResources
		Root      string `json:"host_root"`
		Directory string `json:"host_dir"`
		BinarySHA string `json:"binary_sha256"`
		Residents *struct {
			Records []admission.Resident `json:"records"`
			Error   json.RawMessage      `json:"error"`
		} `json:"residents"`
	}
	if err = json.Unmarshal(raw, &current); err != nil {
		return raw, observation, fmt.Errorf("decode normal-exit parent resources: %w", err)
	}
	if current.Protocol != hostProtocol || current.Rolling != 1 || current.Root != p.Root || current.Directory != filepath.Join(p.Root, "g", p.Source.Instance) || current.PID != p.Native.PID || current.Instance != p.Source.Instance || current.BinarySHA != op.NativeScope.Host.Binary.SHA256 || current.Residents == nil || string(current.Residents.Error) != "null" || len(current.Residents.Records) != 1 {
		return raw, observation, errors.New("normal-exit parent RPC identity or resident persistence differs")
	}
	observation.State = "not_observed"
	if err = admission.VerifyResidentNormalExit(op.Resident, current.Residents.Records[0]); err != nil {
		observation.Diagnostic = err.Error()
	} else {
		observation.State = "observed"
	}
	return raw, observation, nil
}
