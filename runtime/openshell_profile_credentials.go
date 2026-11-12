package runtime

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
)

type openShellCredentialSourceFile struct {
	Gateway   string `json:"gateway"`
	Workspace string `json:"workspace"`
	TLS       struct {
		CAFile          string `json:"ca_file"`
		CertificateFile string `json:"certificate_file"`
		KeyFile         string `json:"key_file"`
		ServerName      string `json:"server_name"`
	} `json:"tls"`
}

func profileCredentialOrigin(source openShellCredentialSourceFile, security OpenShellTLS, ca, cert, key []byte) (OpenShellCredentialSource, error) {
	var result OpenShellCredentialSource
	u, err := url.Parse(source.Gateway)
	if err != nil || u == nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return result, fmt.Errorf("OpenShell credential source endpoint invalid")
	}
	for _, path := range []*string{&source.TLS.CAFile, &source.TLS.CertificateFile, &source.TLS.KeyFile} {
		*path, err = filepath.EvalSymlinks(*path)
		if err != nil {
			return result, fmt.Errorf("OpenShell credential source transport path invalid: %w", err)
		}
	}
	metadata, err := json.Marshal(source)
	if err != nil {
		return result, err
	}
	digest := sha256.New()
	digest.Write([]byte("noodle/credential-source/v1\x00"))
	for _, part := range [][]byte{metadata, ca, cert, key} {
		fmt.Fprintf(digest, "%d\x00", len(part))
		digest.Write(part)
	}
	result = OpenShellCredentialSource{Gateway: source.Gateway, Workspace: source.Workspace, TLS: security, BindingSHA256: hex.EncodeToString(digest.Sum(nil))}
	if err = validateOpenShellCredentialSource(result, ""); err != nil {
		return OpenShellCredentialSource{}, err
	}
	return result, nil
}

func loadProfileCredentialSource(source openShellCredentialSourceFile, read func(string) ([]byte, error)) (OpenShellCredentialSource, error) {
	var empty OpenShellCredentialSource
	ca, err := read(source.TLS.CAFile)
	if err != nil {
		return empty, err
	}
	cert, err := read(source.TLS.CertificateFile)
	if err != nil {
		return empty, err
	}
	key, err := read(source.TLS.KeyFile)
	if err != nil {
		return empty, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return empty, fmt.Errorf("OpenShell credential source CA contains no certificates")
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return empty, fmt.Errorf("OpenShell credential source certificate/key invalid")
	}
	return profileCredentialOrigin(source, OpenShellTLS{RootCAs: roots, Certificate: pair, ServerName: source.TLS.ServerName}, ca, cert, key)
}
