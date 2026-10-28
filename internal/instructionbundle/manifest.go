// Package instructionbundle validates and materializes explicitly admitted
// instruction files. Original source references are identity only, never reads.
package instructionbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

const (
	MaxManifestBytes = 4 << 20
	MaxFiles         = 4096
	MaxFileBytes     = 16 << 20
	MaxTotalBytes    = 64 << 20
	ManifestName     = "manifest.json"
)

type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type File struct {
	Source         FileRef `json:"source"`
	OriginalSource FileRef `json:"original_source"`
	Destination    string  `json:"destination"`
	Mode           *uint32 `json:"mode"`
}

type Binding struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	OrderID       string    `json:"order_id"`
	Files         []File    `json:"files"`
	Bindings      []Binding `json:"bindings"`
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}

func fold(s string) string { return cases.Fold().String(s) }

func privatePath(s string) bool {
	parts := strings.Split(strings.ReplaceAll(s, "\\", "/"), "/")
	for _, part := range parts {
		part = fold(part)
		switch part {
		case ".git", ".ssh", ".gnupg", ".aws", ".env":
			return true
		}
		if strings.HasPrefix(part, ".env.") {
			return true
		}
	}
	leaf := fold(parts[len(parts)-1])
	switch leaf {
	case "auth.json", "credentials", "credentials.json", "credentials.toml":
		return true
	}
	return strings.HasSuffix(leaf, ".pem") || strings.HasSuffix(leaf, ".key")
}

func hasControl(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

// A guest validates host references lexically: it cannot consult the host file
// system. Windows drive and UNC identities remain identities on a Linux guest.
func validateRef(ref FileRef) error {
	s := strings.ReplaceAll(ref.Path, "\\", "/")
	abs := strings.HasPrefix(s, "/") || (len(s) >= 3 && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) && s[1:3] == ":/")
	if !abs || privatePath(s) || !validDigest(ref.SHA256) {
		return fmt.Errorf("instruction file reference invalid or private")
	}
	for _, segment := range strings.Split(s, "/") {
		if segment == ".." {
			return fmt.Errorf("instruction source path contains traversal")
		}
	}
	return nil
}

func validateDestination(s string) error {
	if s == "" || s == "." || path.Clean(s) != s || path.IsAbs(s) || strings.ContainsAny(s, "\\:") || hasControl(s) || privatePath(s) {
		return fmt.Errorf("instruction destination invalid or private: %q", s)
	}
	for _, segment := range strings.Split(s, "/") {
		if segment == ".." || strings.TrimSpace(segment) != segment || strings.HasSuffix(segment, ".") {
			return fmt.Errorf("instruction destination segment invalid: %q", s)
		}
	}
	if fold(strings.Split(s, "/")[0]) == ManifestName {
		return fmt.Errorf("instruction destination uses reserved manifest path")
	}
	return nil
}

func decodeManifest(data []byte, expectedSHA, order string) (*Manifest, error) {
	if len(data) > MaxManifestBytes || !utf8.Valid(data) || !validDigest(expectedSHA) || digest(data) != expectedSHA {
		return nil, fmt.Errorf("instruction manifest size or hash differs")
	}
	if err := manifestJSON(json.NewDecoder(bytes.NewReader(data)), "manifest"); err != nil {
		return nil, fmt.Errorf("instruction manifest JSON invalid: %w", err)
	}
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode instruction manifest: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("instruction manifest has trailing content")
	}
	if m.SchemaVersion != 1 || order == "" || m.OrderID != order || len(m.Files) == 0 || len(m.Files) > MaxFiles {
		return nil, fmt.Errorf("instruction manifest schema, order or count invalid")
	}
	files := map[string]File{}
	folded := map[string]string{}
	directories := map[string]string{}
	for _, f := range m.Files {
		if err := validateRef(f.Source); err != nil {
			return nil, err
		}
		if err := validateRef(f.OriginalSource); err != nil {
			return nil, err
		}
		if err := validateDestination(f.Destination); err != nil {
			return nil, err
		}
		if f.Mode == nil || *f.Mode > 0777 {
			return nil, fmt.Errorf("instruction file mode missing or invalid")
		}
		key := fold(f.Destination)
		if _, ok := folded[key]; ok {
			return nil, fmt.Errorf("instruction destinations collide: %q", f.Destination)
		}
		folded[key] = f.Destination
		files[f.Destination] = f
		for p := path.Dir(f.Destination); p != "."; p = path.Dir(p) {
			key := fold(p)
			if prior, ok := directories[key]; ok && prior != p {
				return nil, fmt.Errorf("instruction directory spellings collide: %q", p)
			}
			directories[key] = p
		}
	}
	for key := range folded {
		if _, ok := directories[key]; ok {
			return nil, fmt.Errorf("instruction file collides with a directory")
		}
	}
	names := map[string]bool{}
	selected := 0
	for _, b := range m.Bindings {
		if b.Name == "" || strings.IndexFunc(b.Name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '-' && r != '_' }) >= 0 || names[fold(b.Name)] {
			return nil, fmt.Errorf("instruction binding alias missing, invalid or duplicated")
		}
		names[fold(b.Name)] = true
		if _, ok := files[b.Path]; !ok {
			return nil, fmt.Errorf("instruction binding path is not an exact file destination")
		}
		switch b.Kind {
		case "selected_skill":
			selected++
			fallthrough
		case "support_skill":
			if path.Base(b.Path) != "SKILL.md" {
				return nil, fmt.Errorf("instruction skill entry is not SKILL.md")
			}
		case "agent", "source":
		default:
			return nil, fmt.Errorf("instruction binding kind unsupported")
		}
	}
	if selected != 1 {
		return nil, fmt.Errorf("instruction manifest does not select exactly one skill")
	}
	return &m, nil
}

// encoding/json accepts case-insensitive struct keys and repeated keys. The
// owner's schema accepts neither; check exact fields before typed decoding.
func manifestJSON(d *json.Decoder, scope string) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		if scope != "" {
			return fmt.Errorf("required JSON container missing")
		}
		return nil
	}
	switch delim {
	case '{':
		var fields []string
		switch scope {
		case "manifest":
			fields = []string{"schema_version", "order_id", "files", "bindings"}
		case "file":
			fields = []string{"source", "original_source", "destination", "mode"}
		case "ref":
			fields = []string{"path", "sha256"}
		case "binding":
			fields = []string{"kind", "name", "path"}
		default:
			return fmt.Errorf("unexpected JSON object")
		}
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			known := false
			for _, field := range fields {
				known = known || s == field
			}
			if !ok || keys[s] || !known {
				return fmt.Errorf("duplicate or invalid object key")
			}
			keys[s] = true
			child := ""
			switch s {
			case "files", "bindings":
				child = s
			case "source", "original_source":
				child = "ref"
			}
			if err := manifestJSON(d, child); err != nil {
				return err
			}
		}
		if len(keys) != len(fields) {
			return fmt.Errorf("required object field missing")
		}
	case '[':
		child := ""
		switch scope {
		case "files":
			child = "file"
		case "bindings":
			child = "binding"
		default:
			return fmt.Errorf("unexpected JSON array")
		}
		count := 0
		for d.More() {
			count++
			if scope == "files" && count > MaxFiles {
				return fmt.Errorf("instruction file count exceeds limit")
			}
			if err := manifestJSON(d, child); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}
