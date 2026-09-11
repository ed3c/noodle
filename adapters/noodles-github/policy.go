package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type requiredCapability struct {
	available bool
	carrier   string
}

var requiredCapabilities = map[string]requiredCapability{
	"static-analysis":             {available: true, carrier: "go vet ./..."},
	"unit-test":                   {available: true, carrier: "go test ./...; pnpm --filter noodle-ui test"},
	"structural":                  {available: true, carrier: "pnpm generate; git diff --exit-code; sh scripts/lint-arch.sh"},
	"security-advisory":           {available: false},
	"runtime-oracle":              {available: true, carrier: "go test ./... -run 'TestNoodlesGitHubTargetConsumer|TestNoodlesDispatchAdmission'"},
	"worktree-execution":          {available: true, carrier: "noodle-ed3c-v0.1.12 worktree create"},
	"github-actions-verification": {available: true, carrier: ".github/workflows/test.yml"},
	"provider-handoff":            {available: true, carrier: "go run ./adapters/noodles-github handoff ed3c/noodle#N"},
	"exact-head-merge":            {available: true, carrier: ".github/workflows/noodles-land.yml"},
}

func loadStrictJSON[T any](path string) (T, error) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil {
		return value, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return value, fmt.Errorf("decode %s trailing data: %w", path, err)
	}
	return value, nil
}

func validatePolicy(policy Policy) error {
	if policy.SchemaVersion != 1 || policy.Repository != targetRepository {
		return fmt.Errorf("target policy must be schema 1 for %s", targetRepository)
	}
	if policy.DefaultBranch != defaultBranch {
		return fmt.Errorf("target policy default branch %q is not %q", policy.DefaultBranch, defaultBranch)
	}
	if policy.RepositoryDispatchSender == "" {
		return fmt.Errorf("target policy repository_dispatch_sender is empty")
	}
	if policy.AuthorizationCommentAuthor == "" {
		return fmt.Errorf("target policy authorization_comment_author is empty")
	}
	if policy.CrossRepositoryStatus != crossRepositoryAdmitted {
		return fmt.Errorf("target policy keeps cross-repository admission held at %q", policy.CrossRepositoryStatus)
	}
	want := []string{targetRepository, sourceRepository}
	got := append([]string(nil), policy.AllowedRepositories...)
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		return fmt.Errorf("target policy allowed_repositories = %v, want exactly %v", policy.AllowedRepositories, want)
	}
	return nil
}

func validateCapabilities(capabilities Capabilities) error {
	if capabilities.SchemaVersion != 1 || capabilities.Repository != targetRepository {
		return fmt.Errorf("capabilities must be schema 1 for %s", targetRepository)
	}
	if len(capabilities.VerificationSurfaces) != len(requiredCapabilities) {
		return fmt.Errorf("capabilities declare %d verification surfaces, want exactly %d", len(capabilities.VerificationSurfaces), len(requiredCapabilities))
	}
	for name, surface := range capabilities.VerificationSurfaces {
		required, ok := requiredCapabilities[name]
		if !ok {
			return fmt.Errorf("capabilities declare unknown verification surface %q", name)
		}
		if surface.Available != required.available {
			return fmt.Errorf("verification surface %q available=%t, want %t", name, surface.Available, required.available)
		}
		if required.carrier == "" {
			if surface.Carrier != nil {
				return fmt.Errorf("unavailable verification surface %q must use a null carrier", name)
			}
			continue
		}
		if surface.Carrier == nil || strings.TrimSpace(*surface.Carrier) != required.carrier {
			return fmt.Errorf("verification surface %q carrier does not match target declaration", name)
		}
	}
	return nil
}
