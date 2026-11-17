package runtime

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/filex"
)

type templatePairingBinary struct {
	Path, SHA256  string
	Bytes         int64
	Version       string `json:"version_stdout"`
	ArchivePolicy int    `json:"archive_policy_schema"`
}

type templatePairingArtifact struct {
	Schema int `json:"schema"`
	Source struct {
		Repository, Head, Tree string
		Clean                  bool
	} `json:"source"`
	Gateway  *templatePairingBinary `json:"gateway"`
	Progress *templatePairingBinary `json:"progress"`
}

type templatePairingInputs struct {
	Template, Gateway, Progress SessionReadbackFileRef
	OriginalPolicy              string
}

type templatePairingReceipt struct {
	Schema  int
	Inputs  templatePairingInputs
	Profile SessionReadbackFileRef
	Policy  string
}

func pairingDigestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, readErr := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), errors.Join(readErr, f.Close())
}

func readTemplatePairingArtifact(ctx context.Context, path string, gateway bool) (SessionReadbackFileRef, templatePairingBinary, error) {
	var manifest templatePairingArtifact
	ref, err := ReadSessionReadbackFile(path, &manifest)
	if err != nil {
		return ref, templatePairingBinary{}, err
	}
	raw, err := os.ReadFile(ref.Path)
	if err != nil {
		return ref, templatePairingBinary{}, err
	}
	if _, err = completionJSON(raw); err != nil {
		return ref, templatePairingBinary{}, fmt.Errorf("template artifact JSON is ambiguous: %w", err)
	}
	validID := func(s string) bool {
		_, err := hex.DecodeString(s)
		return err == nil && (len(s) == 40 || len(s) == 64)
	}
	if manifest.Schema != 1 || !manifest.Source.Clean || !filepath.IsAbs(manifest.Source.Repository) || !validID(manifest.Source.Head) || !validID(manifest.Source.Tree) {
		return ref, templatePairingBinary{}, errors.New("template artifact has no exact clean original source")
	}
	binary := manifest.Progress
	if gateway {
		binary = manifest.Gateway
	}
	if binary == nil || (gateway && manifest.Progress != nil) || (!gateway && manifest.Gateway != nil) {
		return ref, templatePairingBinary{}, errors.New("template artifact producer role differs")
	}
	canonical, err := filepath.EvalSymlinks(binary.Path)
	info, statErr := os.Lstat(binary.Path)
	if err != nil || statErr != nil || !filepath.IsAbs(binary.Path) || canonical != binary.Path || !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 || info.Mode().Perm()&0111 == 0 || binary.Bytes != info.Size() {
		return ref, templatePairingBinary{}, errors.New("template artifact binary is not its immutable regular executable")
	}
	sha, err := pairingDigestFile(binary.Path)
	if err != nil || sha != binary.SHA256 {
		return ref, templatePairingBinary{}, errors.New("template artifact binary differs from original receipt")
	}
	if gateway {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary.Path, "--version")
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return ref, templatePairingBinary{}, err
		}
		if err = cmd.Start(); err != nil {
			return ref, templatePairingBinary{}, err
		}
		output, readErr := io.ReadAll(io.LimitReader(pipe, 4097))
		if len(output) > 4096 || readErr != nil {
			cancel()
		}
		waitErr := cmd.Wait()
		if readErr != nil || waitErr != nil || len(output) > 4096 || !strings.HasPrefix(binary.Version, "openshell-gateway ") || strings.TrimSpace(string(output)) != binary.Version {
			return ref, templatePairingBinary{}, errors.New("template native gateway version differs or is unknown")
		}
	} else {
		info, err := buildinfo.ReadFile(binary.Path)
		settings := map[string]string{}
		if info != nil {
			for _, s := range info.Settings {
				settings[s.Key] = s.Value
			}
		}
		if err != nil || info.Path != "github.com/poteto/noodle/cmd/noodle-progress" || settings["GOOS"] != "linux" || settings["GOARCH"] != "arm64" || settings["CGO_ENABLED"] != "0" || binary.ArchivePolicy != 2 {
			return ref, templatePairingBinary{}, errors.New("template progress artifact is not the qualified Linux archive-policy producer")
		}
	}
	current, err := ReadSessionReadbackFile(ref.Path, nil)
	sha, hashErr := pairingDigestFile(binary.Path)
	if err != nil || current != ref || hashErr != nil || sha != binary.SHA256 {
		return ref, templatePairingBinary{}, errors.New("template artifact changed during readback")
	}
	return ref, *binary, nil
}

// PairGoalOpenShellTemplate publishes only new immutable inputs. Registration,
// Goal selection, credentials and native execution remain with their owners.
func PairGoalOpenShellTemplate(ctx context.Context, root, template, gatewayArtifact, progressArtifact string) (string, error) {
	if gatewayArtifact == "" && progressArtifact == "" {
		return template, nil
	}
	if gatewayArtifact == "" || progressArtifact == "" {
		return "", errors.New("template pairing requires both original artifact receipts")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	original, err := LoadOpenShellProfile(template)
	if err != nil {
		return "", err
	}
	input := templatePairingInputs{OriginalPolicy: original.SourceSHA256}
	input.Template, err = ReadSessionReadbackFile(template, nil)
	if err != nil {
		return "", err
	}
	templateBytes, err := os.ReadFile(input.Template.Path)
	if err != nil {
		return "", err
	}
	if _, err = completionJSON(templateBytes); err != nil {
		return "", fmt.Errorf("template source JSON is ambiguous: %w", err)
	}
	var native, progress templatePairingBinary
	input.Gateway, native, err = readTemplatePairingArtifact(ctx, gatewayArtifact, true)
	if err != nil {
		return "", err
	}
	input.Progress, progress, err = readTemplatePairingArtifact(ctx, progressArtifact, false)
	if err != nil {
		return "", err
	}
	guest := strings.TrimPrefix(original.ProgressSupervisor, "/sandbox/payload/")
	if !filepath.IsLocal(guest) || guest == original.ProgressSupervisor {
		return "", errors.New("template original progress consumer path is unknown")
	}
	if err := pairingPayload(original.PayloadDirectory, "", guest, progress.Path); err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errors.New("template pairing destination is not absolute")
	}
	for ancestor := root; ; ancestor = filepath.Dir(ancestor) {
		_, err := os.Lstat(ancestor)
		if err == nil {
			canonical, err := filepath.EvalSymlinks(ancestor)
			if err != nil || canonical != ancestor {
				return "", errors.New("template pairing destination has a foreign ancestor")
			}
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if canonical, err := filepath.EvalSymlinks(root); err != nil || canonical != root {
		return "", errors.New("template pairing destination has an alias")
	}
	key, err := completionJSONDigest(input)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(root, key)
	profile := filepath.Join(destination, "profile.json")
	if _, err := os.Lstat(destination); err == nil {
		for _, path := range []string{destination, profile, filepath.Join(destination, "pairing.json")} {
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0222 != 0 {
				return "", errors.New("template pairing saved source is writable or foreign")
			}
		}
		var receipt templatePairingReceipt
		if _, err = ReadSessionReadbackFile(filepath.Join(destination, "pairing.json"), &receipt); err != nil {
			return "", fmt.Errorf("template pairing outcome remains unknown: %w", err)
		}
		actual, err := ReadSessionReadbackFile(profile, nil)
		if err != nil || receipt.Schema != 1 || receipt.Inputs != input || actual != receipt.Profile {
			return "", errors.New("template pairing saved source differs")
		}
		loaded, err := LoadOpenShellProfile(profile)
		if err != nil || loaded.SourceSHA256 != receipt.Policy {
			return "", errors.New("template pairing saved dependency changed")
		}
		if err := pairingPayload(original.PayloadDirectory, filepath.Join(destination, "payload"), guest, progress.Path); err != nil {
			return "", err
		}
		return profile, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	staging := destination + ".partial"
	if err := os.Mkdir(staging, 0700); err != nil {
		return "", fmt.Errorf("template pairing partial outcome retained: %w", err)
	}
	// Partial output is retained on failure. Do not erase or rebuild unknown bytes.
	if err := pairingPayload(original.PayloadDirectory, filepath.Join(staging, "payload"), guest, progress.Path); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(input.Template.Path)
	if err != nil {
		return "", err
	}
	raw, err = pairedTemplateBytes(raw, filepath.Dir(template), filepath.Join(destination, "payload"), native.Path, strings.TrimPrefix(native.Version, "openshell-gateway "))
	if err != nil {
		return "", err
	}
	if err = filex.WriteFileAtomicDurable(filepath.Join(staging, "profile.json"), raw); err != nil {
		return "", err
	}
	if err = VerifyOpenShellProfile(original); err != nil {
		return "", err
	}
	if err = VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"template": input.Template, "gateway_artifact": input.Gateway, "progress_artifact": input.Progress}); err != nil {
		return "", err
	}
	for _, artifact := range []templatePairingBinary{native, progress} {
		sha, err := pairingDigestFile(artifact.Path)
		if err != nil || sha != artifact.SHA256 {
			return "", errors.New("template artifact changed before publication")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Publish the complete tree once; the receipt is written only after the
	// original reader has verified paths at their final location.
	if err = os.Rename(staging, destination); err != nil {
		return "", err
	}
	if err = filex.SyncDir(root); err != nil {
		return "", err
	}
	loaded, err := LoadOpenShellProfile(profile)
	if err != nil {
		return "", err
	}
	ref, err := ReadSessionReadbackFile(profile, nil)
	if err != nil {
		return "", err
	}
	receipt := templatePairingReceipt{Schema: 1, Inputs: input, Profile: ref, Policy: loaded.SourceSHA256}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", err
	}
	if err = filex.WriteFileAtomicDurable(filepath.Join(destination, "pairing.json"), data); err != nil {
		return "", err
	}
	for _, p := range []string{profile, filepath.Join(destination, "pairing.json")} {
		if err = os.Chmod(p, 0444); err != nil {
			return "", err
		}
	}
	if err = os.Chmod(destination, 0555); err != nil {
		return "", err
	}
	return profile, nil
}

// Empty destination verifies eligibility without effects. Existing output must
// contain the same immutable hardlinks, not copies or writable shared state.
func pairingPayload(source, destination, progress, binary string) error {
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return errors.New("template original payload source is unavailable")
	}
	// The original profile reader already bound this source, including native
	// temporary-root aliases. Link its physical tree and reverify the profile.
	source = canonical
	create := false
	if destination != "" {
		_, err := os.Lstat(destination)
		create = os.IsNotExist(err)
		if err != nil && !create {
			return err
		}
	}
	var directories []string
	directoryModes := map[string]os.FileMode{}
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0222 != 0 {
			return fmt.Errorf("template payload source is writable: %s", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		selected := path
		if rel == progress {
			selected = binary
			info, err = os.Lstat(binary)
			if err != nil {
				return err
			}
		}
		if destination == "" {
			return nil
		}
		target := filepath.Join(destination, rel)
		if !create {
			current, err := os.Lstat(target)
			if err != nil || current.Mode() != info.Mode() {
				return errors.New("template payload hardlink mode or source changed")
			}
			if info.Mode().IsRegular() && !os.SameFile(info, current) {
				return errors.New("template payload no longer uses its original immutable inode")
			}
			if info.Mode()&os.ModeSymlink != 0 {
				old, e := os.Readlink(selected)
				now, n := os.Readlink(target)
				if e != nil || n != nil || old != now {
					return errors.New("template payload link changed")
				}
			}
			return nil
		}
		switch {
		case info.IsDir():
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			directories = append(directories, target)
			directoryModes[target] = info.Mode().Perm()
		case info.Mode().IsRegular():
			return os.Link(selected, target)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(selected)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return errors.New("template payload source has an unsupported entry")
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err = os.Chmod(directories[i], directoryModes[directories[i]]); err != nil {
			return err
		}
		if err = filex.SyncDir(directories[i]); err != nil {
			return err
		}
	}
	return nil
}

func pairedTemplateBytes(raw []byte, base, payload, binary, version string) ([]byte, error) {
	var f openShellProfileFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	f.CLI, f.PayloadDirectory = resolve(f.CLI), payload
	f.Gateway.Resident.Argv[0] = binary
	f.Gateway.Resident.Directory = resolve(f.Gateway.Resident.Directory)
	f.Gateway.Version = version
	f.Gateway.IdentitySource = resolve(f.Gateway.IdentitySource)
	f.Gateway.Binding.StateRoot = resolve(f.Gateway.Binding.StateRoot)
	for i, p := range f.Gateway.ConfigFiles {
		f.Gateway.ConfigFiles[i] = resolve(p)
	}
	for name, route := range f.Routes {
		route.SpecFile = resolve(route.SpecFile)
		f.Routes[name] = route
	}
	for _, p := range []*string{&f.Gateway.TLS.CAFile, &f.Gateway.TLS.CertificateFile, &f.Gateway.TLS.KeyFile} {
		*p = resolve(*p)
	}
	if f.CredentialSource != nil {
		for _, p := range []*string{&f.CredentialSource.TLS.CAFile, &f.CredentialSource.TLS.CertificateFile, &f.CredentialSource.TLS.KeyFile} {
			*p = resolve(*p)
		}
	}
	// LoadOpenShellProfile already rejected every unmodelled original field.
	return json.Marshal(f)
}
