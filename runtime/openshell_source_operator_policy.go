package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// This candidate is a profile transition, not a custody revision, shutdown
// receipt, execution permission or proof that the native policy is active.
type SourceOperatorPolicyCandidate struct {
	Schema                      int
	BeforeSource                string
	Before, After               SessionReadbackFileRef
	BeforeBinding, AfterBinding OpenShellCustodyBinding
}

// Resolve only the existing profile's host-file references. Guest paths, argv,
// environment and resident cwd already have their original runtime meaning.
func sourceOperatorProfile(data []byte, source string) (openShellProfileFile, error) {
	var f openShellProfileFile
	if err := json.Unmarshal(data, &f); err != nil {
		return f, err
	}
	resolve := func(value string) string {
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(filepath.Dir(source), value)
	}
	f.CLI, f.PayloadDirectory = resolve(f.CLI), resolve(f.PayloadDirectory)
	for key, route := range f.Routes {
		route.SpecFile = resolve(route.SpecFile)
		f.Routes[key] = route
	}
	f.Gateway.IdentitySource = resolve(f.Gateway.IdentitySource)
	f.Gateway.Binding.StateRoot = resolve(f.Gateway.Binding.StateRoot)
	for i, path := range f.Gateway.ConfigFiles {
		f.Gateway.ConfigFiles[i] = resolve(path)
	}
	for _, path := range []*string{&f.Gateway.TLS.CAFile, &f.Gateway.TLS.CertificateFile, &f.Gateway.TLS.KeyFile} {
		*path = resolve(*path)
	}
	if f.CredentialSource != nil {
		for _, path := range []*string{&f.CredentialSource.TLS.CAFile, &f.CredentialSource.TLS.CertificateFile, &f.CredentialSource.TLS.KeyFile} {
			*path = resolve(*path)
		}
	}
	return f, nil
}

func sourceOperatorFlagAbsent(argv []string) bool {
	for _, arg := range argv {
		if arg == "--enable-operator-auth" || strings.HasPrefix(arg, "--enable-operator-auth=") {
			return false
		}
	}
	return true
}

// InspectSourceOperatorPolicy requires the only semantic change to be the
// explicit operator flag. It uses gateway-only validation, never worker payload.
func InspectSourceOperatorPolicy(before, after SessionReadbackFileRef, beforeSource string) (SourceOperatorPolicyCandidate, error) {
	result := SourceOperatorPolicyCandidate{Schema: 1, Before: before, After: after, BeforeSource: beforeSource}
	if before.Path == after.Path {
		return result, errors.New("Source operator candidate must preserve the original profile")
	}
	if err := VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"before": before, "after": after}); err != nil {
		return result, err
	}
	// The existing loader includes lexical dependency paths in its fingerprint.
	// Preserve the configured source spelling while binding its canonical bytes.
	original, err := ReadSessionReadbackFile(beforeSource, nil)
	if err != nil || original != before {
		return result, errors.New("Source operator profile source differs from its original file binding")
	}
	old, err := loadOpenShellProfile(beforeSource, true)
	if err != nil {
		return result, err
	}
	updated, err := loadOpenShellProfile(after.Path, true)
	if err != nil {
		return result, err
	}
	if old.sourceFiles[old.SourcePath] != before.SHA256 || updated.sourceFiles[updated.SourcePath] != after.SHA256 {
		return result, errors.New("Source operator loader observed different profile bytes")
	}
	read := func(path string, ref SessionReadbackFileRef) (openShellProfileFile, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return openShellProfileFile{}, err
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != ref.SHA256 {
			return openShellProfileFile{}, errors.New("Source operator profile changed before semantic comparison")
		}
		return sourceOperatorProfile(data, path)
	}
	left, err := read(beforeSource, before)
	if err != nil {
		return result, err
	}
	right, err := read(after.Path, after)
	if err != nil {
		return result, err
	}
	if left.CredentialSource != nil || !sourceOperatorFlagAbsent(left.Gateway.Resident.Argv) {
		return result, errors.New("Source operator revision requires the original Source profile without an explicit operator flag or credential-source binding")
	}
	want := append(append([]string{}, left.Gateway.Resident.Argv...), "--enable-operator-auth", "true")
	if !reflect.DeepEqual(right.Gateway.Resident.Argv, want) {
		return result, errors.New("Source operator candidate changes resident arguments beyond the one operator flag")
	}
	right.Gateway.Resident.Argv = left.Gateway.Resident.Argv
	if !reflect.DeepEqual(left, right) {
		return result, errors.New("Source operator candidate changes another profile policy or dependency")
	}
	delete(old.sourceFiles, old.SourcePath)
	delete(updated.sourceFiles, updated.SourcePath)
	if !reflect.DeepEqual(old.sourceFiles, updated.sourceFiles) || old.ClientSHA256 != updated.ClientSHA256 {
		return result, errors.New("Source operator profile dependencies changed during validation")
	}
	result.BeforeBinding, result.AfterBinding = old.Gateway.Binding, updated.Gateway.Binding
	unchanged := result.AfterBinding
	unchanged.ConfigFingerprint = result.BeforeBinding.ConfigFingerprint
	if unchanged != result.BeforeBinding || result.BeforeBinding.ConfigFingerprint == result.AfterBinding.ConfigFingerprint {
		return result, errors.New("Source operator candidate did not preserve the original deployment with a changed policy fingerprint")
	}
	original, err = ReadSessionReadbackFile(beforeSource, nil)
	if err != nil || original != before {
		return result, errors.New("Source operator profile source changed during validation")
	}
	return result, VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"before": before, "after": after})
}

// PrepareSourceOperatorPolicy creates an immutable candidate artifact. It does
// not change the original profile, custody, native DB, process or refresh grant.
func PrepareSourceOperatorPolicy(before SessionReadbackFileRef, beforeSource, destination string) (SourceOperatorPolicyCandidate, error) {
	var result SourceOperatorPolicyCandidate
	if !filepath.IsAbs(destination) {
		return result, errors.New("Source operator profile destination must be absolute")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return result, err
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	if destination == before.Path {
		return result, errors.New("Source operator candidate cannot overwrite its original profile")
	}
	if err = VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"before": before}); err != nil {
		return result, err
	}
	original, err := ReadSessionReadbackFile(beforeSource, nil)
	if err != nil || original != before {
		return result, errors.New("Source operator profile source differs from its original file binding")
	}
	if _, err = LoadOpenShellGatewaySpec(beforeSource); err != nil {
		return result, err
	}
	data, err := os.ReadFile(before.Path)
	if err != nil {
		return result, err
	}
	f, err := sourceOperatorProfile(data, beforeSource)
	if err != nil {
		return result, err
	}
	if f.CredentialSource != nil || !sourceOperatorFlagAbsent(f.Gateway.Resident.Argv) {
		return result, errors.New("original Source already has an explicit operator policy or credential-source binding")
	}
	f.Gateway.Resident.Argv = append(f.Gateway.Resident.Argv, "--enable-operator-auth", "true")
	data, err = json.MarshalIndent(f, "", "  ")
	if err != nil {
		return result, err
	}
	data = append(data, '\n')
	prior, err := os.ReadFile(destination)
	if err == nil {
		if string(prior) != string(data) {
			return result, errors.New("Source operator profile destination already contains different bytes")
		}
	} else if !os.IsNotExist(err) {
		return result, err
	} else if err = maintenanceWriteBytes(destination, data); err != nil {
		return result, err
	}
	after, err := ReadSessionReadbackFile(destination, nil)
	if err != nil {
		return result, err
	}
	return InspectSourceOperatorPolicy(before, after, beforeSource)
}
