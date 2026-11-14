package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

// Selection is read-only provenance, never native retirement or authority to
// consume the internal accounting API. Other schemas retain their own owner.
type hostSourceQuotaSnapshot struct {
	Ledger         loopruntime.SessionReadbackFileRef   `json:"ledger"`
	Schema         int                                  `json:"schema"`
	ResidentReturn *admission.SourceResidentQuotaReturn `json:"resident_return,omitempty"`
}

func observeSourceQuota(op hostOperatorPlan) (*hostSourceQuotaSnapshot, error) {
	var source struct {
		Root     string `json:"host_root"`
		Instance string `json:"host_instance_id"`
	}
	if err := json.Unmarshal(op.Source, &source); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(source.Root) || source.Instance == "" {
		return nil, errors.New("Source quota lacks its original Host binding")
	}
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(source.Root, "quota", "quota.json"), nil)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(ref.Path)
	if err != nil || hostDigest(raw) != ref.SHA256 {
		return nil, errors.New("original Source quota changed during selection")
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err = json.Unmarshal(raw, &header); err != nil {
		return nil, err
	}
	if header.Schema <= 0 {
		return nil, errors.New("original Source quota schema is missing or invalid")
	}
	result := &hostSourceQuotaSnapshot{Ledger: ref, Schema: header.Schema}
	if header.Schema == 2 {
		selected, err := admission.PrepareSourceResidentQuotaReturn(raw, source.Instance, op.Resident.Contexts, op.Resident.ID)
		if err != nil {
			return nil, err
		}
		result.ResidentReturn = &selected
	}
	if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"source-quota": ref}); err != nil {
		return nil, err
	}
	return result, nil
}
