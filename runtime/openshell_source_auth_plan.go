package runtime

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

type SourceAuthAccess struct {
	Gateway                 string `json:"gateway"`
	Workspace               string `json:"workspace"`
	ServerName              string `json:"server_name"`
	TrustSHA256             string `json:"trust_sha256"`
	ClientCertificateSHA256 string `json:"client_certificate_sha256"`
	BindingSHA256           string `json:"binding_sha256"`
}
type SourceAuthPending struct {
	Provider              string `json:"provider"`
	SourceBinding         string `json:"source_binding"`
	SourceReadPending     bool   `json:"source_read_pending"`
	TargetMutationPending bool   `json:"target_mutation_pending"`
	TargetReceiptCount    int    `json:"target_receipt_count"`
}
type SourceAuthPlan struct {
	Schema               int                               `json:"schema"`
	Status               string                            `json:"status"`
	Executable           bool                              `json:"executable"`
	Effects              []string                          `json:"effects"`
	Sources              map[string]SessionReadbackFileRef `json:"sources"`
	ContextID            string                            `json:"context_id"`
	OrderID              string                            `json:"order_id"`
	CanonicalAttemptID   string                            `json:"canonical_attempt_id"`
	AttemptID            string                            `json:"native_attempt_id"`
	SessionID            string                            `json:"session_id"`
	WorktreePath         string                            `json:"worktree_path"`
	FrozenProfileSHA256  string                            `json:"frozen_profile_sha256"`
	TemplatePolicySHA256 string                            `json:"template_policy_sha256"`
	CustodyVersion       int                               `json:"custody_version"`
	CurrentAccess        SourceAuthAccess                  `json:"current_access"`
	ProposedAccess       SourceAuthAccess                  `json:"proposed_access"`
	PreservedPending     []SourceAuthPending               `json:"preserved_pending"`
	Requires             []string                          `json:"requires"`
}

// InspectSourceAuthPlan proposes only client authentication. Original profile,
// custody, ciphertext/AAD and receipts remain bound bytes, never after-images.
func InspectSourceAuthPlan(profilePath, templatePath, dispatchPath, contextID, controlRoot string) (result SourceAuthPlan, err error) {
	result = SourceAuthPlan{Schema: 1, Status: "refused", Effects: []string{}, Sources: map[string]SessionReadbackFileRef{}, PreservedPending: []SourceAuthPending{}}
	original, err := LoadOpenShellProfile(profilePath)
	if err != nil {
		return result, err
	}
	candidate, err := LoadOpenShellProfile(templatePath)
	if err != nil {
		return result, err
	}
	for prefix, profile := range map[string]OpenShellProfile{"original": original, "template": candidate} {
		for path, digest := range profile.sourceFiles {
			ref, err := ReadSessionReadbackFile(path, nil)
			if err != nil || ref.SHA256 != digest {
				return result, fmt.Errorf("Source auth profile dependency changed: %s", path)
			}
			result.Sources[prefix+":"+path] = ref
		}
	}
	result.TemplatePolicySHA256 = candidate.SourceSHA256
	result.Sources["template_profile"] = result.Sources["template:"+candidate.SourcePath]
	readSource := func(profile OpenShellProfile, template bool) (openShellCredentialSourceFile, error) {
		data, err := os.ReadFile(profile.SourcePath)
		if err != nil {
			return openShellCredentialSourceFile{}, err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != profile.sourceFiles[profile.SourcePath] {
			return openShellCredentialSourceFile{}, fmt.Errorf("Source auth profile changed before semantic read")
		}
		file, err := sourceOperatorProfile(data, profile.SourcePath)
		if err != nil {
			return openShellCredentialSourceFile{}, err
		}
		if file.CredentialSource == nil {
			if !template {
				return openShellCredentialSourceFile{}, fmt.Errorf("original credential Source is unavailable")
			}
			origin := openShellCredentialSourceFile{Gateway: file.Gateway.Binding.Gateway, Workspace: file.Gateway.Binding.Workspace}
			origin.TLS = file.Gateway.TLS
			return origin, nil
		}
		return *file.CredentialSource, nil
	}
	before, err := readSource(original, false)
	if err != nil {
		return result, err
	}
	after, err := readSource(candidate, true)
	if err != nil {
		return result, err
	}
	access := func(file openShellCredentialSourceFile, profile OpenShellProfile) (SourceAuthAccess, error) {
		trust, err := ReadSessionReadbackFile(file.TLS.CAFile, nil)
		if err != nil {
			return SourceAuthAccess{}, err
		}
		if trust.SHA256 != profile.sourceFiles[file.TLS.CAFile] {
			return SourceAuthAccess{}, fmt.Errorf("Source server trust changed before semantic read")
		}
		cert := profile.CredentialOrigin.TLS.Certificate.Certificate
		if len(cert) == 0 {
			return SourceAuthAccess{}, fmt.Errorf("Source client certificate is unavailable")
		}
		sum := sha256.Sum256(cert[0])
		return SourceAuthAccess{file.Gateway, file.Workspace, file.TLS.ServerName, trust.SHA256, hex.EncodeToString(sum[:]), profile.CredentialOrigin.BindingSHA256}, nil
	}
	result.CurrentAccess, err = access(before, original)
	if err != nil {
		return result, err
	}
	result.ProposedAccess, err = access(after, candidate)
	if err != nil {
		return result, err
	}
	if before.Gateway != after.Gateway || before.Workspace != after.Workspace || before.TLS.ServerName != after.TLS.ServerName || result.CurrentAccess.TrustSHA256 != result.ProposedAccess.TrustSHA256 {
		return result, fmt.Errorf("Source auth proposal changes endpoint, workspace or server trust")
	}
	security := candidate.CredentialOrigin.TLS
	leaf, err := x509.ParseCertificate(security.Certificate.Certificate[0])
	if err != nil {
		return result, err
	}
	if !slices.Contains(leaf.Subject.OrganizationalUnit, "operator") {
		return result, fmt.Errorf("proposed Source client is not an operator")
	}
	chain := x509.NewCertPool()
	for _, der := range security.Certificate.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return result, err
		}
		chain.AddCert(cert)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: security.RootCAs, Intermediates: chain, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return result, fmt.Errorf("proposed Source client trust: %w", err)
	}
	var receipt openShellDispatchReceipt
	result.Sources["dispatch"], err = ReadSessionReadbackFile(dispatchPath, &receipt)
	if err != nil {
		return result, err
	}
	dispatchSHA, err := OpenShellDispatchSHA256(original)
	if err != nil {
		return result, err
	}
	if contextID == "" || receipt.Version != 1 || receipt.ContextID != contextID || receipt.ProfileSHA256 != dispatchSHA || receipt.OrderID == "" || receipt.SessionID == "" || receipt.AttemptID == "" || !filepath.IsAbs(receipt.Target) {
		return result, fmt.Errorf("original dispatch identity or Frozen profile differs")
	}
	var state OpenShellCustodyState
	result.Sources["custody"], err = ReadSessionReadbackFile(filepath.Join(original.Gateway.Binding.StateRoot, "owner.json"), &state)
	if err != nil {
		return result, err
	}
	if (state.Version != openShellCustodyVersion && state.Version != openShellCredentialCustodyVersion) || state.Binding != original.Gateway.Binding || controlRoot == "" || state.ControlRoot != controlRoot {
		return result, fmt.Errorf("original custody schema or business binding differs")
	}
	if state.Handover != nil {
		if err = verifyCustodyHandover(*state.Handover, state); err != nil {
			return result, err
		}
		result.Sources["custody_handover"] = *state.Handover
	}
	for _, record := range state.Records {
		if record.Retained || (record.Task.SessionID != receipt.SessionID && record.Task.AttemptID != receipt.AttemptID) {
			continue
		}
		if record.Task != (OpenShellTask{receipt.OrderID, receipt.AttemptID, receipt.SessionID}) {
			return result, fmt.Errorf("original native attempt contradicts custody task")
		}
		if record.CreateAcknowledged {
			identity := record.Worker.Identity
			if identity.OrderID != receipt.OrderID || identity.AttemptID != receipt.AttemptID || identity.SessionID != receipt.SessionID {
				return result, fmt.Errorf("original native attempt contradicts worker identity")
			}
		}
	}
	ledger := OpenShellCustody{state: state}
	if err = ledger.validateLedger(); err != nil {
		return result, err
	}
	for _, row := range state.Credentials {
		if row.SourceBinding != original.CredentialOrigin.BindingSHA256 {
			return result, fmt.Errorf("Frozen credential Source binding differs")
		}
		result.PreservedPending = append(result.PreservedPending, SourceAuthPending{row.Provider, row.SourceBinding, row.SourceReadPending, row.Pending != nil, len(row.TargetReceipts)})
	}
	result.ContextID, result.OrderID, result.AttemptID, result.SessionID, result.WorktreePath = contextID, receipt.OrderID, receipt.AttemptID, receipt.SessionID, receipt.Target
	result.FrozenProfileSHA256, result.CustodyVersion = dispatchSHA, state.Version
	// A readable proposal is not a no-effect ACK or a supported live handoff.
	result.Requires = []string{"original_owner_auth_handoff_capability", "original_pending_effect_readback", "current_effect_authorization"}
	result.Status = "unknown"
	if err = VerifySessionReadbackFiles(result.Sources); err != nil {
		result.Status = "refused"
		return result, err
	}
	return result, nil
}
