package runtime

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/poteto/noodle/internal/filex"
)

type GoalProfileReceipt struct {
	Schema               int                    `json:"schema"`
	Template             SessionReadbackFileRef `json:"template"`
	Profile              SessionReadbackFileRef `json:"profile"`
	HostRoot             string                 `json:"host_root"`
	Gateway              string                 `json:"gateway"`
	TemplatePolicySHA256 string                 `json:"template_policy_sha256"`
	Files                map[string]string      `json:"files"`
}

// ReadGoalOpenShellReservation reads the saved endpoint, not dispatch readiness.
func ReadGoalOpenShellReservation(hostRoot string) (GoalProfileReceipt, error) {
	var result GoalProfileReceipt
	if !filepath.IsAbs(hostRoot) || filepath.Clean(hostRoot) != hostRoot {
		return result, errors.New("Goal Host root is not an absolute canonical path")
	}
	if canonical, e := filepath.EvalSymlinks(hostRoot); e != nil || canonical != hostRoot {
		return result, errors.New("Goal Host root contains an unresolved or foreign symlink")
	}
	destination := filepath.Join(hostRoot, "openshell")
	if _, err := ReadSessionReadbackFile(filepath.Join(destination, "materialization.json"), &result); err != nil {
		return result, err
	}
	if result.Schema != 1 || result.HostRoot != hostRoot || result.Profile.Path != filepath.Join(destination, "profile.json") || result.Files["profile.json"] != result.Profile.SHA256 {
		return result, errors.New("original Goal profile reservation binding differs")
	}
	var document openShellProfileFile
	profile, err := ReadSessionReadbackFile(result.Profile.Path, &document)
	if err != nil {
		return result, err
	}
	if profile != result.Profile || document.SchemaVersion != 1 || document.Gateway.Binding.StateRoot != filepath.Join(destination, "custody") || document.Gateway.Binding.Gateway == "" || document.Gateway.Binding.Gateway != result.Gateway {
		return result, errors.New("original Goal profile endpoint differs from its materialization")
	}
	return result, nil
}

// ReadGoalOpenShellProfile validates the original configuration without creating missing files.
func ReadGoalOpenShellProfile(hostRoot string) (GoalProfileReceipt, error) {
	result, err := ReadGoalOpenShellReservation(hostRoot)
	if err != nil {
		return result, err
	}
	destination := filepath.Join(hostRoot, "openshell")
	source, err := ReadSessionReadbackFile(result.Template.Path, nil)
	if err != nil {
		return result, err
	}
	original, err := LoadOpenShellProfile(source.Path)
	if err != nil {
		return result, err
	}
	if result.Schema != 1 || result.Template != source || result.HostRoot != hostRoot || result.Profile.Path != filepath.Join(destination, "profile.json") || result.TemplatePolicySHA256 != original.SourceSHA256 || len(result.Files) == 0 {
		return result, errors.New("original Goal profile materialization binding differs")
	}
	for relative, digest := range result.Files {
		if !filepath.IsLocal(relative) {
			return result, errors.New("original Goal profile file destination escaped")
		}
		ref, e := ReadSessionReadbackFile(filepath.Join(destination, relative), nil)
		if e != nil {
			return result, e
		}
		if ref.SHA256 != digest {
			return result, errors.New("original Goal profile dependency changed")
		}
	}
	profile, err := ReadSessionReadbackFile(result.Profile.Path, nil)
	if err != nil || profile != result.Profile {
		return result, errors.New("original Goal profile source changed")
	}
	loaded, err := LoadOpenShellProfile(profile.Path)
	if err == nil && loaded.Gateway.Binding.Gateway != result.Gateway {
		err = errors.New("original Goal profile endpoint differs from its materialization")
	}
	return result, err
}

// MaterializeGoalOpenShellProfile creates configuration, never a gateway or VM.
func MaterializeGoalOpenShellProfile(template, hostRoot, address string) (GoalProfileReceipt, error) {
	var result GoalProfileReceipt
	if !filepath.IsAbs(hostRoot) || filepath.Clean(hostRoot) != hostRoot {
		return result, errors.New("Goal Host root is not an absolute canonical path")
	}
	if canonical, e := filepath.EvalSymlinks(hostRoot); e != nil || canonical != hostRoot {
		return result, errors.New("Goal Host root contains an unresolved or foreign symlink")
	}
	source, err := ReadSessionReadbackFile(template, nil)
	if err != nil {
		return result, err
	}
	template = source.Path
	original, err := LoadOpenShellProfile(template)
	if err != nil {
		return result, err
	}
	destination := filepath.Join(hostRoot, "openshell")
	if _, err = os.Lstat(destination); err == nil {
		result, err = ReadGoalOpenShellProfile(hostRoot)
		if err == nil && (result.Template != source || result.TemplatePolicySHA256 != original.SourceSHA256 || address != "" && result.Gateway != "https://"+address) {
			err = errors.New("original Goal profile materialization binding differs")
		}
		return result, err
	} else if !os.IsNotExist(err) {
		return result, err
	}
	var f openShellProfileFile
	raw, err := os.ReadFile(template)
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		return result, err
	}
	resolve := func(value string) string {
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		return filepath.Join(filepath.Dir(template), value)
	}
	if f.CredentialSource == nil {
		f.CredentialSource = &openShellCredentialSourceFile{Gateway: f.Gateway.Binding.Gateway, Workspace: f.Gateway.Binding.Workspace}
		f.CredentialSource.TLS = f.Gateway.TLS
	}
	for _, value := range []*string{&f.CredentialSource.TLS.CAFile, &f.CredentialSource.TLS.CertificateFile, &f.CredentialSource.TLS.KeyFile} {
		*value = resolve(*value)
	}
	if len(f.Gateway.ConfigFiles) != 1 || resolve(f.Gateway.ConfigFiles[0]) != resolve(f.Gateway.IdentitySource) {
		return result, errors.New("Goal profile template does not select one gateway configuration source")
	}
	configuration, err := os.ReadFile(resolve(f.Gateway.IdentitySource))
	if err != nil {
		return result, err
	}
	var document map[string]any
	if _, err = toml.Decode(string(configuration), &document); err != nil {
		return result, fmt.Errorf("decode Goal gateway template: %w", err)
	}
	openshell, ok := document["openshell"].(map[string]any)
	if !ok || openshell["version"] != int64(2) {
		return result, errors.New("Goal gateway template does not use OpenShell configuration version 2")
	}
	gateway, ok := openshell["gateway"].(map[string]any)
	if !ok {
		return result, errors.New("Goal gateway configuration is absent")
	}
	drivers, ok := openshell["drivers"].(map[string]any)
	if !ok {
		return result, errors.New("Goal gateway driver configuration is absent")
	}
	vm, ok := drivers["vm"].(map[string]any)
	if !ok || f.Gateway.ComputeDriver != "vm" {
		return result, errors.New("Goal gateway template is not a VM deployment")
	}
	if address == "" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return result, err
		}
		address = listener.Addr().String()
		if err = listener.Close(); err != nil {
			return result, err
		}
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return result, err
	}
	number, err := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || number <= 0 || number > 65535 {
		return result, errors.New("Goal profile endpoint is not a selected loopback port")
	}
	endpoint := "https://" + address
	name := "noodle-" + filepath.Base(hostRoot)
	staging, err := os.MkdirTemp(hostRoot, ".openshell-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(staging)
	files := make(map[string]string)
	write := func(relative string, data []byte) error {
		digest := sha256.Sum256(data)
		files[relative] = hex.EncodeToString(digest[:])
		path := filepath.Join(staging, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := filex.WriteFileAtomicDurable(path, data); err != nil {
			return err
		}
		return os.Chmod(path, 0600)
	}
	if err = goalProfileKeys(name, write); err != nil {
		return result, err
	}
	path := func(relative string) string { return filepath.Join(destination, relative) }
	gateway["name"], gateway["bind_address"] = name, address
	gateway["guest_tls_ca"] = path("tls/ca.crt")
	credential, ok := gateway["credential_storage"].(map[string]any)
	if !ok {
		credential = map[string]any{}
		gateway["credential_storage"] = credential
	}
	credential["key_encryption_key_path"] = path("keys/credential.key")
	jwt, ok := gateway["gateway_jwt"].(map[string]any)
	if !ok {
		jwt = map[string]any{}
		gateway["gateway_jwt"] = jwt
	}
	jwt["signing_key_path"], jwt["public_key_path"], jwt["kid_path"], jwt["gateway_id"] = path("keys/signing.pem"), path("keys/public.pem"), path("keys/kid"), name
	vm["state_dir"] = path("vm")
	var encoded bytes.Buffer
	if err = toml.NewEncoder(&encoded).Encode(document); err != nil {
		return result, err
	}
	if err = write("gateway.toml", encoded.Bytes()); err != nil {
		return result, err
	}
	clientRoot := path("client")
	stateRoot := path("gateway-state")
	database := "sqlite://" + path("gateway.db") + "?mode=rwc"
	remapEnvironment := func(environment []string) []string {
		values := map[string]string{"XDG_CONFIG_HOME": clientRoot, "XDG_STATE_HOME": stateRoot, "OPENSHELL_DB_URL": database}
		result := make([]string, 0, len(environment)+3)
		for _, value := range environment {
			key, _, _ := strings.Cut(value, "=")
			if replacement, ok := values[key]; ok {
				result = append(result, key+"="+replacement)
				delete(values, key)
			} else {
				result = append(result, value)
			}
		}
		for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "OPENSHELL_DB_URL"} {
			if value, ok := values[key]; ok {
				result = append(result, key+"="+value)
			}
		}
		return result
	}
	resident := f.Gateway.Resident
	arguments := append([]string(nil), resident.Argv...)
	if len(arguments) == 0 {
		return result, errors.New("Goal gateway foreground executable is absent")
	}
	arguments[0] = resolve(arguments[0])
	required := map[string]string{"--config": path("gateway.toml"), "--tls-cert": path("tls/server.crt"), "--tls-key": path("tls/server.key"), "--tls-client-ca": path("tls/ca.crt")}
	for i := 1; i < len(arguments); i++ {
		if arguments[i] == "--db-url" {
			if i+1 >= len(arguments) {
				return result, errors.New("Goal gateway database argument is incomplete")
			}
			arguments[i+1] = database
			i++
			continue
		}
		if value, ok := required[arguments[i]]; ok {
			if i+1 >= len(arguments) {
				return result, errors.New("Goal gateway template has incomplete configuration arguments")
			}
			arguments[i+1] = value
			delete(required, arguments[i])
			i++
			continue
		}
		return result, fmt.Errorf("Goal gateway template has an unsupported foreground argument: %s", arguments[i])
	}
	if len(required) != 0 {
		return result, errors.New("Goal gateway template omits its configuration or TLS arguments")
	}
	f.CLI, f.PayloadDirectory = resolve(f.CLI), resolve(f.PayloadDirectory)
	for name, route := range f.Routes {
		route.SpecFile = resolve(route.SpecFile)
		f.Routes[name] = route
	}
	f.GatewayName = name
	f.Environment = remapEnvironment(f.Environment)
	f.Gateway.Resident.Name = name
	f.Gateway.Resident.Argv = arguments
	f.Gateway.Resident.Directory = destination
	f.Gateway.Resident.Environment = remapEnvironment(resident.Environment)
	f.Gateway.Binding.StateRoot = path("custody")
	f.Gateway.Binding.Gateway = endpoint
	f.Gateway.Binding.Workspace = "default"
	f.Gateway.Binding.Owner = name
	f.Gateway.IdentitySource = path("gateway.toml")
	f.Gateway.ConfigFiles = []string{path("gateway.toml")}
	f.Gateway.Retained = nil
	f.Gateway.TLS.CAFile = path("tls/ca.crt")
	f.Gateway.TLS.CertificateFile = path("tls/client.crt")
	f.Gateway.TLS.KeyFile = path("tls/client.key")
	f.Gateway.TLS.ServerName = ""
	metadata, err := json.Marshal(map[string]any{"name": name, "gateway_endpoint": endpoint, "is_remote": false, "gateway_port": number, "auth_mode": "mtls"})
	if err != nil {
		return result, err
	}
	client := filepath.Join("client", "openshell", "gateways", name)
	if err = write(filepath.Join(client, "metadata.json"), metadata); err != nil {
		return result, err
	}
	for _, item := range []struct{ from, to string }{{"tls/ca.crt", "ca.crt"}, {"tls/client.crt", "tls.crt"}, {"tls/client.key", "tls.key"}} {
		data, readErr := os.ReadFile(filepath.Join(staging, item.from))
		if readErr != nil {
			return result, readErr
		}
		if err = write(filepath.Join(client, "mtls", item.to), data); err != nil {
			return result, err
		}
	}
	profile, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return result, err
	}
	if err = write("profile.json", profile); err != nil {
		return result, err
	}
	digest := sha256.Sum256(profile)
	result = GoalProfileReceipt{Schema: 1, Template: source, Profile: SessionReadbackFileRef{Path: path("profile.json"), SHA256: hex.EncodeToString(digest[:])}, HostRoot: hostRoot, Gateway: endpoint, TemplatePolicySHA256: original.SourceSHA256, Files: files}
	receipt, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return result, err
	}
	if err = write("materialization.json", receipt); err != nil {
		return result, err
	}
	if err = os.Rename(staging, destination); err != nil {
		return result, err
	}
	if err = filex.SyncDir(hostRoot); err != nil {
		return result, err
	}
	if _, err = LoadOpenShellProfile(result.Profile.Path); err != nil {
		return result, err
	}
	return result, nil
}

func goalProfileKeys(name string, write func(string, []byte) error) error {
	serial := func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
	now := time.Now()
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	number, err := serial()
	if err != nil {
		return err
	}
	ca := &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: name + " CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		return err
	}
	if err = write("tls/ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})); err != nil {
		return err
	}
	for _, role := range []string{"server", "client"} {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		number, e := serial()
		if e != nil {
			return e
		}
		certificate := &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: name + " " + role}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature}
		if role == "server" {
			certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			certificate.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			certificate.DNSNames = []string{"localhost"}
		} else {
			certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, e := x509.CreateCertificate(rand.Reader, certificate, ca, key.Public(), caKey)
		if e != nil {
			return e
		}
		encoded, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			return e
		}
		if e = write("tls/"+role+".crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); e != nil {
			return e
		}
		if e = write("tls/"+role+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})); e != nil {
			return e
		}
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return err
	}
	if err = write("keys/signing.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})); err != nil {
		return err
	}
	if err = write("keys/public.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})); err != nil {
		return err
	}
	kid := sha256.Sum256(publicDER)
	if err = write("keys/kid", []byte(hex.EncodeToString(kid[:]))); err != nil {
		return err
	}
	credential := make([]byte, 32)
	if _, err = rand.Read(credential); err != nil {
		return err
	}
	return write("keys/credential.key", credential)
}
