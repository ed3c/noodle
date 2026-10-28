package runtime

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"google.golang.org/protobuf/encoding/protojson"
)

// OpenShellProfile snapshots dispatch policy and client TLS identity. Model
// credentials remain in the gateway; the JSON source contains only paths and policy.
type OpenShellProfile struct {
	Gateway                         OpenShellGatewaySpec
	CLI, GatewayName                string
	Environment                     []string
	PayloadDirectory, PayloadSHA256 string
	ClientSHA256                    string
	InstructionVerifier             string
	Routes                          map[string]OpenShellRoute
}
type OpenShellRoute struct {
	Binary string
	Spec   *pb.SandboxSpec
}

type openShellProfileFile struct {
	SchemaVersion       int      `json:"schema_version"`
	CLI                 string   `json:"cli"`
	GatewayName         string   `json:"gateway_name"`
	Environment         []string `json:"environment"`
	PayloadDirectory    string   `json:"payload_directory"`
	InstructionVerifier string   `json:"instruction_verifier"`
	Routes              map[string]struct {
		Binary   string `json:"binary"`
		SpecFile string `json:"spec_file"`
	} `json:"routes"`
	Gateway struct {
		Resident admission.ResidentSpec `json:"resident"`
		Binding  struct {
			StateRoot string `json:"state_root"`
			Gateway   string `json:"gateway"`
			Workspace string `json:"workspace"`
			Owner     string `json:"owner"`
		} `json:"binding"`
		TLS struct {
			CAFile          string `json:"ca_file"`
			CertificateFile string `json:"certificate_file"`
			KeyFile         string `json:"key_file"`
			ServerName      string `json:"server_name"`
		} `json:"tls"`
		Version                 string                     `json:"version"`
		ComputeDriver           string                     `json:"compute_driver"`
		ReadinessTimeoutSeconds int                        `json:"readiness_timeout_seconds"`
		ConfigFiles             []string                   `json:"config_files"`
		Retained                []OpenShellRetainedSandbox `json:"retained"`
	} `json:"gateway"`
}

func LoadOpenShellProfile(filename string) (OpenShellProfile, error) {
	var p OpenShellProfile
	if !filepath.IsAbs(filename) {
		return p, fmt.Errorf("OpenShell profile path is not absolute")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return p, fmt.Errorf("read OpenShell profile: %w", err)
	}
	var f openShellProfileFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&f); err != nil {
		return p, fmt.Errorf("decode OpenShell profile: %w", err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return p, fmt.Errorf("OpenShell profile has trailing content")
	}
	if f.SchemaVersion != 1 || strings.TrimSpace(f.GatewayName) == "" || len(f.Routes) == 0 {
		return p, fmt.Errorf("OpenShell profile schema, gateway name or routes invalid")
	}
	resolve := func(value string) (string, error) {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("OpenShell profile path missing")
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(filepath.Dir(filename), value)
		}
		return filepath.Clean(value), nil
	}
	hash := sha256.New()
	hash.Write(data)
	read := func(value string) ([]byte, error) {
		name, e := resolve(value)
		if e != nil {
			return nil, e
		}
		b, e := os.ReadFile(name)
		if e != nil {
			return nil, fmt.Errorf("read OpenShell profile dependency %s: %w", name, e)
		}
		fmt.Fprintf(hash, "\x00%s\x00%d\x00", name, len(b))
		hash.Write(b)
		return b, nil
	}
	if p.CLI, err = resolve(f.CLI); err != nil {
		return p, err
	}
	if info, e := os.Stat(p.CLI); e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return p, fmt.Errorf("OpenShell CLI executable unavailable")
	}
	if _, err = read(f.CLI); err != nil {
		return p, err
	}
	p.GatewayName = f.GatewayName
	p.Environment = append([]string(nil), f.Environment...)
	if p.PayloadDirectory, err = resolve(f.PayloadDirectory); err != nil {
		return p, err
	}
	if filepath.Base(p.PayloadDirectory) != "payload" {
		return p, fmt.Errorf("OpenShell payload directory basename is not payload")
	}
	if p.PayloadSHA256, err = openShellPayloadHash(p.PayloadDirectory); err != nil {
		return p, err
	}
	hash.Write([]byte(p.PayloadSHA256))
	if f.InstructionVerifier != "" {
		if !path.IsAbs(f.InstructionVerifier) || path.Clean(f.InstructionVerifier) != f.InstructionVerifier || strings.ContainsRune(f.InstructionVerifier, 0) || !strings.HasPrefix(f.InstructionVerifier, "/sandbox/payload/") {
			return p, fmt.Errorf("OpenShell instruction verifier is outside the guest payload")
		}
		localVerifier := filepath.Join(p.PayloadDirectory, filepath.FromSlash(strings.TrimPrefix(f.InstructionVerifier, "/sandbox/payload/")))
		info, e := os.Lstat(localVerifier)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return p, fmt.Errorf("OpenShell instruction verifier executable unavailable in payload")
		}
		p.InstructionVerifier = f.InstructionVerifier
	}
	p.Routes = make(map[string]OpenShellRoute, len(f.Routes))
	// JSON map order is intentionally irrelevant; include routes in a fixed order.
	for key := range f.Routes {
		if key != "codex" && key != "pi" {
			return p, fmt.Errorf("OpenShell harness route %q unsupported", key)
		}
	}
	for _, key := range []string{"codex", "pi"} {
		r, ok := f.Routes[key]
		if !ok {
			continue
		}
		if !path.IsAbs(r.Binary) || path.Clean(r.Binary) != r.Binary || strings.ContainsRune(r.Binary, 0) || !strings.HasPrefix(r.Binary, "/sandbox/payload/") {
			return p, fmt.Errorf("OpenShell %s binary is outside the guest payload", key)
		}
		guestBinary := filepath.Join(p.PayloadDirectory, filepath.FromSlash(strings.TrimPrefix(r.Binary, "/sandbox/payload/")))
		if info, e := os.Stat(guestBinary); e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return p, fmt.Errorf("OpenShell %s guest executable unavailable in payload", key)
		}
		b, e := read(r.SpecFile)
		if e != nil {
			return p, e
		}
		spec := new(pb.SandboxSpec)
		if e = protojson.Unmarshal(b, spec); e != nil {
			return p, fmt.Errorf("decode OpenShell %s sandbox specification: %w", key, e)
		}
		if len(spec.GetProviders()) != 1 || spec.GetTemplate().GetImage() == "" {
			return p, fmt.Errorf("OpenShell %s provider or image missing", key)
		}
		p.Routes[key] = OpenShellRoute{Binary: r.Binary, Spec: spec}
	}
	ca, err := read(f.Gateway.TLS.CAFile)
	if err != nil {
		return p, err
	}
	cert, err := read(f.Gateway.TLS.CertificateFile)
	if err != nil {
		return p, err
	}
	key, err := read(f.Gateway.TLS.KeyFile)
	if err != nil {
		return p, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return p, fmt.Errorf("OpenShell gateway CA contains no certificates")
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return p, fmt.Errorf("OpenShell client certificate/key invalid: %w", err)
	}
	for _, name := range f.Gateway.ConfigFiles {
		if _, err = read(name); err != nil {
			return p, err
		}
	}
	stateRoot, err := resolve(f.Gateway.Binding.StateRoot)
	if err != nil {
		return p, err
	}
	resident := f.Gateway.Resident
	if err = admission.ValidateResidentSpec(resident); err != nil {
		return p, err
	}
	if resident.Sharing != "shared" || resident.ForceAfterGrace {
		return p, fmt.Errorf("OpenShell gateway resident is not guarded shared foreground")
	}
	if f.Gateway.ReadinessTimeoutSeconds <= 0 || f.Gateway.ReadinessTimeoutSeconds > 30 {
		return p, fmt.Errorf("OpenShell gateway readiness timeout outside 1..30 seconds")
	}
	p.Gateway = OpenShellGatewaySpec{Resident: resident, Binding: OpenShellCustodyBinding{StateRoot: stateRoot, Gateway: f.Gateway.Binding.Gateway, Workspace: f.Gateway.Binding.Workspace, Owner: f.Gateway.Binding.Owner, ConfigSource: filename, ConfigFingerprint: hex.EncodeToString(hash.Sum(nil))}, TLS: OpenShellTLS{RootCAs: roots, Certificate: pair, ServerName: f.Gateway.TLS.ServerName}, Version: f.Gateway.Version, ComputeDriver: f.Gateway.ComputeDriver, ReadinessTimeout: time.Duration(f.Gateway.ReadinessTimeoutSeconds) * time.Second, Retained: append([]OpenShellRetainedSandbox(nil), f.Gateway.Retained...)}
	if p.ClientSHA256, err = openShellClientHash(p); err != nil {
		return p, err
	}
	hash.Write([]byte(p.ClientSHA256))
	p.Gateway.Binding.ConfigFingerprint = hex.EncodeToString(hash.Sum(nil))
	return p, nil
}

func VerifyOpenShellPayload(p OpenShellProfile) error {
	digest, err := openShellPayloadHash(p.PayloadDirectory)
	if err != nil {
		return err
	}
	if digest != p.PayloadSHA256 || p.PayloadSHA256 == "" {
		return fmt.Errorf("OpenShell payload changed after profile admission")
	}
	return nil
}
func openShellPayloadHash(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("OpenShell payload root is not a directory")
	}
	hash := sha256.New()
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, e := filepath.Rel(root, name)
		if e != nil {
			return e
		}
		i, e := entry.Info()
		if e != nil {
			return e
		}
		fmt.Fprintf(hash, "%s\x00%o\x00", filepath.ToSlash(rel), i.Mode())
		switch {
		case entry.IsDir():
			return nil
		case i.Mode().IsRegular():
			f, e := os.Open(name)
			if e != nil {
				return e
			}
			defer f.Close()
			digest := sha256.New()
			if _, e = io.Copy(digest, f); e != nil {
				return e
			}
			hash.Write(digest.Sum(nil))
		case i.Mode()&os.ModeSymlink != 0:
			target, e := os.Readlink(name)
			if e != nil {
				return e
			}
			resolved, e := filepath.EvalSymlinks(name)
			if e != nil {
				return e
			}
			under, e := filepath.Rel(root, resolved)
			if e != nil || under == ".." || strings.HasPrefix(under, ".."+string(filepath.Separator)) || filepath.IsAbs(target) {
				return fmt.Errorf("OpenShell payload symlink escapes payload: %s", rel)
			}
			hash.Write([]byte(target))
		default:
			return fmt.Errorf("OpenShell payload special file unsupported: %s", rel)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint OpenShell payload: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
