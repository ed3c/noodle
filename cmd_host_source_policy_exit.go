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
	State      string `json:"state"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

func readSourcePolicyStatus(ctx context.Context, p hostMaintenancePlan) (json.RawMessage, sourcePolicyNormalExit, error) {
	observation := sourcePolicyNormalExit{State: "unknown"}
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		return nil, observation, err
	}
	checkParent := func() error {
		if p.Native == nil || !p.Native.SameGeneration(op.NativeScope.Host.Process) {
			return errors.New("normal-exit observation lacks its original Host generation")
		}
		current, err := procx.ObserveNativeIdentity(p.Native.PID)
		if err != nil || !p.Native.SameGeneration(current) || current.Zombie {
			return errors.New("normal-exit observation lost its original live parent")
		}
		return nil
	}
	if err = checkParent(); err != nil {
		return nil, observation, err
	}
	raw, err := nativeHostRPC(ctx, filepath.Join(p.Root, "g", p.Source.Instance), hostRequest{Operation: "resources"})
	if err != nil {
		return raw, observation, err
	}
	if err = checkParent(); err != nil {
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
	if op.Resident.PID != op.NativeScope.Gateway.Process.PID {
		return raw, observation, errors.New("normal-exit resident differs from the original gateway actor")
	}
	observation.State = "not_observed"
	if err = admission.VerifyResidentNormalExit(op.Resident, current.Residents.Records[0]); err != nil {
		observation.Diagnostic = err.Error()
	} else {
		observation.State = "observed"
	}
	return raw, observation, nil
}
