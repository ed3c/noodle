package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
)

type hostBootstrap struct {
	Schema          int             `json:"schema"`
	Phase           string          `json:"phase"`
	Target          string          `json:"target"`
	TargetSHA       string          `json:"target_sha256"`
	Source          hostGeneration  `json:"source"`
	ProcessIdentity string          `json:"process_identity"`
	Tasks           json.RawMessage `json:"tasks"`
}

func saveHostBootstrap(root string, b hostBootstrap) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(root, "bootstrap.json"), raw)
}

func readHostBootstrap(root string) (hostBootstrap, error) {
	var b hostBootstrap
	raw, err := os.ReadFile(filepath.Join(root, "bootstrap.json"))
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.Schema != 1 || !validPoolID(b.Source.ID) || b.Source.Directory != filepath.Join(root, "g", b.Source.ID) || b.Phase == "" || !filepath.IsAbs(b.Target) || b.TargetSHA == "" || b.ProcessIdentity == "" {
		return b, errors.New("original Host maintenance binding is invalid")
	}
	return b, nil
}

type legacyHostResources struct {
	Protocol  string `json:"protocol"`
	Instance  string `json:"host_instance_id"`
	PID       int    `json:"host_pid"`
	Contexts  int    `json:"contexts"`
	Rolling   int    `json:"rolling_host_version"`
	Resources struct {
		Workers     int `json:"active_workers"`
		Residents   int `json:"active_residents"`
		Allocations int `json:"active_allocations"`
		Unresolved  int `json:"unresolved_residents"`
	} `json:"resources"`
	Residents struct {
		Error any `json:"error"`
	} `json:"residents"`
}

func readLegacyResources(ctx context.Context, dir string) (legacyHostResources, error) {
	var r legacyHostResources
	raw, err := nativeHostRPC(ctx, dir, hostRequest{Operation: "resources"})
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	var required map[string]json.RawMessage
	if err = json.Unmarshal(raw, &required); err != nil {
		return r, err
	}
	if r.Protocol != hostProtocol || !validPoolID(r.Instance) || r.PID <= 0 || r.Rolling != 0 || required["resources"] == nil {
		return r, errors.New("original legacy Host resource identity is unavailable")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(required["resources"], &fields); err != nil {
		return r, err
	}
	for _, name := range []string{"active_workers", "active_residents", "active_allocations", "unresolved_residents"} {
		if fields[name] == nil {
			return r, fmt.Errorf("legacy Host omits %s", name)
		}
	}
	return r, nil
}

// This compatibility path is bound to the demonstrated old reducer/control
// implementation. An unknown build must expose a native migration contract.
func verifyLegacyBootstrapBinary(path, digest string) error {
	raw, err := os.ReadFile(path)
	if err != nil || hostDigest(raw) != digest {
		return errors.New("original legacy executable changed")
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return err
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["vcs.revision"] != "2b636db20166ada9e304ac3462db52dd07c69899" || settings["vcs.modified"] != "false" {
		return errors.New("legacy executable has no verified first-migration contract")
	}
	return nil
}

func bootstrapTaskRequest(t hostBootstrapTask) hostRequest {
	r := t.Original
	return hostRequest{ProjectDir: r.ProjectDir, ContextID: r.ContextID, InstanceID: r.HostInstanceID, ConfigPath: r.ConfigPath, ConfigSHA256: r.ConfigSHA256, Mode: r.Mode, RequestID: r.RequestID, CleanupCompleted: r.CleanupCompleted, BinarySHA256: r.BinarySHA256, AdmissionSHA: r.BinarySHA256}
}

type hostBootstrapTask struct {
	Original hostReceipt `json:"original"`
}
