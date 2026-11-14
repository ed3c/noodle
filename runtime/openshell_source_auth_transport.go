package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/admission"
)

// NewOpenShellSourceAuthGateway freezes a registered operator candidate for the
// original Source credential channel. Logical bindings and Target TLS remain
// those of profilePath. Construction reserves ordinary custody without RPCs.
// The maintenance caller still owns fresh retirement, lease and task admission;
// neither the historical proposal nor this factory proves those conditions.
// A non-nil owner on error has the ordinary constructor's Close obligation.
func NewOpenShellSourceAuthGateway(residents *admission.Residents, budget *admission.Budget, profilePath string, plan SourceAuthPlan, controlRoot string) (*OpenShellGateway, error) {
	if plan.Schema != 1 || plan.Status != "unknown" || plan.Executable || len(plan.Effects) != 0 {
		return nil, fmt.Errorf("Source auth proposal shape differs")
	}
	for _, name := range []string{"dispatch", "custody"} {
		ref := plan.Sources[name]
		digest, err := hex.DecodeString(ref.SHA256)
		if !filepath.IsAbs(ref.Path) || err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("Source auth historical %s reference invalid", name)
		}
	}
	var registration struct {
		Schema       int                    `json:"schema"`
		Profile      SessionReadbackFileRef `json:"profile"`
		PolicySHA256 string                 `json:"policy_sha256"`
	}
	registered := plan.Sources["template_registration"]
	current, err := ReadSessionReadbackFile(registered.Path, &registration)
	if err != nil {
		return nil, err
	}
	if current != registered || registration.Schema != 1 || registration.Profile != plan.Sources["registered_template"] || registration.Profile != plan.Sources["template_profile"] || registration.PolicySHA256 != plan.TemplatePolicySHA256 {
		return nil, fmt.Errorf("Source auth registered candidate differs")
	}
	// Only immutable policy inputs are hash fences. Dispatch and custody refs
	// describe history; legitimate progression can change their bytes and schema.
	immutable := map[string]SessionReadbackFileRef{"registration": registered, "candidate": registration.Profile}
	for name, ref := range plan.Sources {
		if strings.HasPrefix(name, "original:") || strings.HasPrefix(name, "template:") {
			immutable[name] = ref
		}
	}
	if err := VerifySessionReadbackFiles(immutable); err != nil {
		return nil, err
	}
	verified, err := InspectSourceAuthPlan(profilePath, registration.Profile.Path, plan.Sources["dispatch"].Path, plan.ContextID, controlRoot)
	if err != nil {
		return nil, err
	}
	if plan.CurrentAccess != verified.CurrentAccess || plan.ProposedAccess != verified.ProposedAccess || plan.TemplatePolicySHA256 != verified.TemplatePolicySHA256 || plan.FrozenProfileSHA256 != verified.FrozenProfileSHA256 || plan.ContextID != verified.ContextID || plan.OrderID != verified.OrderID || plan.AttemptID != verified.AttemptID || plan.SessionID != verified.SessionID || plan.WorktreePath != verified.WorktreePath {
		return nil, fmt.Errorf("Source auth proposal differs from original profile or task")
	}
	dependencies := make(map[string]SessionReadbackFileRef)
	for name, ref := range verified.Sources {
		if strings.HasPrefix(name, "original:") || strings.HasPrefix(name, "template:") {
			dependencies[name] = ref
		}
	}
	expected := maps.Clone(immutable)
	delete(expected, "registration")
	delete(expected, "candidate")
	if !maps.Equal(expected, dependencies) || plan.Sources["dispatch"].Path != verified.Sources["dispatch"].Path || plan.Sources["custody"].Path != verified.Sources["custody"].Path {
		return nil, fmt.Errorf("Source auth original dependency references differ")
	}
	original, err := LoadOpenShellProfile(profilePath)
	if err != nil {
		return nil, err
	}
	candidate, err := LoadOpenShellProfile(registration.Profile.Path)
	if err != nil {
		return nil, err
	}
	if original.Gateway.CredentialSource == nil || original.CredentialOrigin.BindingSHA256 != verified.CurrentAccess.BindingSHA256 || candidate.CredentialOrigin.BindingSHA256 != verified.ProposedAccess.BindingSHA256 || candidate.SourceSHA256 != verified.TemplatePolicySHA256 {
		return nil, fmt.Errorf("Source auth loaded transport differs")
	}
	dispatchSHA, err := OpenShellDispatchSHA256(original)
	if err != nil {
		return nil, err
	}
	if dispatchSHA != verified.FrozenProfileSHA256 || plan.Sources["custody"].Path != filepath.Join(original.Gateway.Binding.StateRoot, "owner.json") {
		return nil, fmt.Errorf("Source auth Frozen profile or custody source differs")
	}
	effective, err := freezeOpenShellTLS(candidate.CredentialOrigin.TLS)
	if err != nil {
		return nil, err
	}
	if err := VerifyOpenShellProfile(original); err != nil {
		return nil, err
	}
	if err := VerifyOpenShellProfile(candidate); err != nil {
		return nil, err
	}
	if err := VerifySessionReadbackFiles(immutable); err != nil {
		return nil, err
	}
	spec := original.Gateway
	spec.ControlRoot = controlRoot
	owner, err := NewOpenShellGateway(residents, budget, spec)
	if owner != nil {
		owner.credentialTLS = &effective
	}
	return owner, err
}
