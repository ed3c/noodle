package runtime

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
)

// A historical resource receipt binds its original profile, not today's payload.
// The saved native snapshot is evidence only when it recreates that exact digest.
func verifyStorageFailureHistoricalProfile(profile OpenShellProfile, expected string) error {
	var snapshot struct {
		Schema   int `json:"schema"`
		Profiles []struct {
			Path          string                  `json:"path"`
			SourceSHA256  string                  `json:"profile_sha256"`
			PayloadSHA256 string                  `json:"payload_sha256"`
			ClientSHA256  string                  `json:"client_sha256"`
			Gateway       OpenShellCustodyBinding `json:"gateway_binding"`
		} `json:"profiles"`
	}
	path := filepath.Join(filepath.Dir(profile.SourcePath), "static-validation.json")
	if _, err := ReadSessionReadbackFile(path, &snapshot); err != nil {
		return fmt.Errorf("storage failure historical profile snapshot unavailable: %w", err)
	}
	if snapshot.Schema != 1 {
		return fmt.Errorf("storage failure historical profile snapshot schema differs")
	}
	matched := 0
	for _, row := range snapshot.Profiles {
		if row.Path != profile.SourcePath {
			continue
		}
		matched++
		if row.Gateway != profile.Gateway.Binding || row.ClientSHA256 != profile.ClientSHA256 {
			return fmt.Errorf("storage failure historical gateway or client differs")
		}
		for _, value := range []string{row.SourceSHA256, row.PayloadSHA256} {
			b, err := hex.DecodeString(value)
			if err != nil || len(b) != 32 {
				return fmt.Errorf("storage failure historical profile digest invalid")
			}
		}
		historical := profile
		historical.SourceSHA256, historical.PayloadSHA256 = row.SourceSHA256, row.PayloadSHA256
		digest, err := OpenShellDispatchSHA256(historical)
		if err != nil {
			return err
		}
		if digest != expected {
			return fmt.Errorf("storage failure historical profile does not match frozen dispatch")
		}
	}
	if matched != 1 {
		return fmt.Errorf("storage failure historical profile snapshot is missing or ambiguous")
	}
	return nil
}
