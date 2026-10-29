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
	"google.golang.org/protobuf/proto"
)

// OpenShellProfile snapshots dispatch policy and client TLS identity. Model
// credentials remain in the gateway; the JSON source contains only paths and policy.
type OpenShellProfile struct {
	sourceFiles                     map[string]string
	SourcePath, SourceSHA256        string
	Gateway                         OpenShellGatewaySpec
	CLI, GatewayName                string
	Environment                     []string
	PayloadDirectory, PayloadSHA256 string
	ClientSHA256                    string
	InstructionVerifier             string
	ProgressSupervisor              string
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
	ProgressSupervisor  string   `json:"progress_supervisor"`
	Routes              map[string]struct {
		Binary   string `json:"binary"`
		SpecFile string `json:"spec_file"`
	} `json:"routes"`
	Gateway struct {
		IdentitySource string                 `json:"identity_source"`
		Resident       admission.ResidentSpec `json:"resident"`
		Binding        struct {
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
	gatewayHash := sha256.New()
	profileBytes := sha256.Sum256(data)
	p.sourceFiles = map[string]string{filename: hex.EncodeToString(profileBytes[:])}
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
		digest := sha256.Sum256(b)
		sourceSHA := hex.EncodeToString(digest[:])
		if old, ok := p.sourceFiles[name]; ok && old != sourceSHA {
			return nil, fmt.Errorf("OpenShell dependency changed during profile admission")
		}
		p.sourceFiles[name] = sourceSHA
		fmt.Fprintf(hash, "\x00%s\x00%d\x00", name, len(b))
		hash.Write(b)
		return b, nil
	}
	readGateway := func(value string) ([]byte, error) {
		b, e := read(value)
		if e != nil {
			return nil, e
		}
		name, e := resolve(value)
		if e != nil {
			return nil, e
		}
		fmt.Fprintf(gatewayHash, "\x00%s\x00%d\x00", name, len(b))
		gatewayHash.Write(b)
		return b, nil
	}
	if p.CLI, err = resolve(f.CLI); err != nil {
		return p, err
	}
	if info, e := os.Stat(p.CLI); e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return p, fmt.Errorf("OpenShell CLI executable unavailable")
	}
	if _, err = readGateway(f.CLI); err != nil {
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
	if !path.IsAbs(f.ProgressSupervisor) || path.Clean(f.ProgressSupervisor) != f.ProgressSupervisor || strings.ContainsRune(f.ProgressSupervisor, 0) || !strings.HasPrefix(f.ProgressSupervisor, "/sandbox/payload/") {
		return p, fmt.Errorf("OpenShell progress supervisor is missing or outside the guest payload")
	}
	localSupervisor := filepath.Join(p.PayloadDirectory, filepath.FromSlash(strings.TrimPrefix(f.ProgressSupervisor, "/sandbox/payload/")))
	info, statErr := os.Lstat(localSupervisor)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return p, fmt.Errorf("OpenShell progress supervisor executable unavailable in payload")
	}
	p.ProgressSupervisor = f.ProgressSupervisor
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
	ca, err := readGateway(f.Gateway.TLS.CAFile)
	if err != nil {
		return p, err
	}
	cert, err := readGateway(f.Gateway.TLS.CertificateFile)
	if err != nil {
		return p, err
	}
	key, err := readGateway(f.Gateway.TLS.KeyFile)
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
	identitySource, err := resolve(f.Gateway.IdentitySource)
	if err != nil {
		return p, fmt.Errorf("OpenShell gateway identity source missing: %w", err)
	}
	identitySource, err = filepath.EvalSymlinks(identitySource)
	if err != nil {
		return p, fmt.Errorf("OpenShell gateway identity source unavailable: %w", err)
	}
	foundIdentity := false
	for i, name := range f.Gateway.ConfigFiles {
		resolved, e := resolve(name)
		if e != nil {
			return p, e
		}
		if _, e = read(resolved); e != nil {
			return p, e
		}
		resolved, e = filepath.EvalSymlinks(resolved)
		if e != nil {
			return p, e
		}
		f.Gateway.ConfigFiles[i] = resolved
		if resolved == identitySource {
			foundIdentity = true
		}
		if _, err = readGateway(resolved); err != nil {
			return p, err
		}
	}
	if !foundIdentity {
		return p, fmt.Errorf("OpenShell gateway identity source is absent from static configuration files")
	}
	stateRoot, err := resolve(f.Gateway.Binding.StateRoot)
	if err != nil {
		return p, err
	}
	f.Gateway.IdentitySource = identitySource
	f.Gateway.Binding.StateRoot = stateRoot
	for _, value := range []*string{&f.Gateway.TLS.CAFile, &f.Gateway.TLS.CertificateFile, &f.Gateway.TLS.KeyFile} {
		*value, err = resolve(*value)
		if err != nil {
			return p, err
		}
	}
	deployment, err := json.Marshal(struct {
		Gateway     any
		CLI, Name   string
		Environment []string
	}{f.Gateway, p.CLI, p.GatewayName, p.Environment})
	if err != nil {
		return p, err
	}
	gatewayHash.Write(deployment)
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
	p.Gateway = OpenShellGatewaySpec{Resident: resident, Binding: OpenShellCustodyBinding{StateRoot: stateRoot, Gateway: f.Gateway.Binding.Gateway, Workspace: f.Gateway.Binding.Workspace, Owner: f.Gateway.Binding.Owner, ConfigSource: identitySource, ConfigFingerprint: hex.EncodeToString(gatewayHash.Sum(nil))}, TLS: OpenShellTLS{RootCAs: roots, Certificate: pair, ServerName: f.Gateway.TLS.ServerName}, Version: f.Gateway.Version, ComputeDriver: f.Gateway.ComputeDriver, ReadinessTimeout: time.Duration(f.Gateway.ReadinessTimeoutSeconds) * time.Second, Retained: append([]OpenShellRetainedSandbox(nil), f.Gateway.Retained...)}
	if p.ClientSHA256, err = openShellClientHash(p); err != nil {
		return p, err
	}
	hash.Write([]byte(p.ClientSHA256))
	gatewayHash.Write([]byte(p.ClientSHA256))
	p.Gateway.Binding.ConfigFingerprint = hex.EncodeToString(gatewayHash.Sum(nil))
	p.SourcePath = filename
	p.SourceSHA256 = hex.EncodeToString(hash.Sum(nil))
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

// File profiles retain their entire original dependency hash independently of
// the shared gateway's identity. A changed task policy never rebinds custody.
func VerifyOpenShellProfile(p OpenShellProfile) error {
	if p.SourcePath == "" {
		return VerifyOpenShellPayload(p)
	}
	if p.SourceSHA256 == "" || p.sourceFiles[p.SourcePath] == "" {
		return fmt.Errorf("OpenShell task profile source binding missing")
	}
	// Check only admitted paths. Re-decoding a changed profile would follow new,
	// unadmitted dependency paths before it could detect the source change.
	for name, expected := range p.sourceFiles {
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("OpenShell task profile source unavailable: %s", name)
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, readErr := io.Copy(hash, file)
		closeErr := file.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected {
			return fmt.Errorf("OpenShell task profile changed after admission: %s", name)
		}
	}
	if err := VerifyOpenShellPayload(p); err != nil {
		return err
	}
	return VerifyOpenShellClient(p)
}
func freezeOpenShellProfile(p OpenShellProfile) OpenShellProfile {
	p.Environment = append([]string(nil), p.Environment...)
	files := make(map[string]string, len(p.sourceFiles))
	for name, hash := range p.sourceFiles {
		files[name] = hash
	}
	p.sourceFiles = files
	routes := make(map[string]OpenShellRoute, len(p.Routes))
	for name, route := range p.Routes {
		if route.Spec != nil {
			route.Spec = proto.Clone(route.Spec).(*pb.SandboxSpec)
		}
		routes[name] = route
	}
	p.Routes = routes
	return p
}

// OpenShellDispatchSHA256 binds the complete frozen worker profile, including
// its original file dependency digest when it was loaded from disk.
func OpenShellDispatchSHA256(p OpenShellProfile) (string, error) {
	type routeBinding struct {
		Binary string
		Spec   []byte
	}
	routes := make(map[string]routeBinding, len(p.Routes))
	for name, route := range p.Routes {
		if route.Spec == nil {
			return "", fmt.Errorf("OpenShell route specification missing")
		}
		spec, err := openShellBytes(route.Spec)
		if err != nil {
			return "", err
		}
		routes[name] = routeBinding{route.Binary, spec}
	}
	data, err := json.Marshal(struct {
		SourcePath, SourceSHA256                                                                                 string
		Gateway                                                                                                  OpenShellCustodyBinding
		CLI, GatewayName, PayloadDirectory, PayloadSHA256, ClientSHA256, InstructionVerifier, ProgressSupervisor string
		Environment                                                                                              []string
		Routes                                                                                                   map[string]routeBinding
	}{p.SourcePath, p.SourceSHA256, p.Gateway.Binding, p.CLI, p.GatewayName, p.PayloadDirectory, p.PayloadSHA256, p.ClientSHA256, p.InstructionVerifier, p.ProgressSupervisor, p.Environment, routes})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
