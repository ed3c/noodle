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
	"os"
	"path/filepath"
	"strings"
)

// VerifyOpenShellClient binds native name-based file transfer to the same
// endpoint and mTLS identity as the gRPC custody owner. Active selection and
// last-sandbox files are irrelevant because every operation passes its name.
func VerifyOpenShellClient(p OpenShellProfile) error {
	digest, err := openShellClientHash(p)
	if err != nil {
		return err
	}
	if p.ClientSHA256 == "" || digest != p.ClientSHA256 {
		return fmt.Errorf("OpenShell CLI gateway selection changed after admission")
	}
	return nil
}
func openShellClientHash(p OpenShellProfile) (string, error) {
	configRoot := ""
	seen := make(map[string]bool)
	for _, entry := range p.Environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || seen[key] || strings.ContainsRune(entry, 0) {
			return "", fmt.Errorf("OpenShell CLI environment invalid or duplicate")
		}
		seen[key] = true
		if key == "XDG_CONFIG_HOME" {
			configRoot = value
		}
	}
	if !filepath.IsAbs(configRoot) {
		return "", fmt.Errorf("OpenShell CLI XDG_CONFIG_HOME is not explicitly absolute")
	}
	if filepath.Base(p.GatewayName) != p.GatewayName || p.GatewayName == "." || p.GatewayName == ".." || p.GatewayName == "" {
		return "", fmt.Errorf("OpenShell CLI gateway name invalid")
	}
	root := filepath.Join(configRoot, "openshell", "gateways", p.GatewayName)
	hash := sha256.New()
	cli, err := os.Open(p.CLI)
	if err != nil {
		return "", fmt.Errorf("open OpenShell CLI executable: %w", err)
	}
	defer cli.Close()
	info, err := cli.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("OpenShell CLI executable unavailable")
	}
	fmt.Fprintf(hash, "cli\x00%s\x00%o\x00", p.CLI, info.Mode())
	digest := sha256.New()
	if _, err = io.Copy(digest, cli); err != nil {
		return "", fmt.Errorf("fingerprint OpenShell CLI executable: %w", err)
	}
	hash.Write(digest.Sum(nil))
	read := func(rel string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return nil, fmt.Errorf("read OpenShell CLI %s: %w", rel, err)
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", rel, len(data))
		hash.Write(data)
		return data, nil
	}
	data, err := read("metadata.json")
	if err != nil {
		return "", err
	}
	var metadata struct {
		Name     string `json:"name"`
		Endpoint string `json:"gateway_endpoint"`
		AuthMode string `json:"auth_mode"`
	}
	if err = json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("OpenShell CLI gateway metadata invalid: %w", err)
	}
	if metadata.Name != p.GatewayName || metadata.Endpoint != p.Gateway.Binding.Gateway || metadata.AuthMode != "mtls" {
		return "", fmt.Errorf("OpenShell CLI gateway selection conflicts with custody endpoint or authentication")
	}
	ca, err := read("mtls/ca.crt")
	if err != nil {
		return "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) || p.Gateway.TLS.RootCAs == nil || !roots.Equal(p.Gateway.TLS.RootCAs) {
		return "", fmt.Errorf("OpenShell CLI CA conflicts with custody trust")
	}
	certificate, err := read("mtls/tls.crt")
	if err != nil {
		return "", err
	}
	key, err := read("mtls/tls.key")
	if err != nil {
		return "", err
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return "", fmt.Errorf("OpenShell CLI mTLS identity invalid: %w", err)
	}
	expected := p.Gateway.TLS.Certificate.Certificate
	if len(pair.Certificate) != len(expected) {
		return "", fmt.Errorf("OpenShell CLI client certificate conflicts with custody identity")
	}
	for i := range expected {
		if !bytes.Equal(pair.Certificate[i], expected[i]) {
			return "", fmt.Errorf("OpenShell CLI client certificate conflicts with custody identity")
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
