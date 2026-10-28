package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type contextFileRef = loopruntime.SessionReadbackFileRef

type contextOwnerIdentity struct {
	InstanceID   string `json:"instance_id"`
	BinarySHA256 string `json:"binary_sha256"`
	HostDir      string `json:"host_dir"`
}

type contextLauncher struct {
	Python      contextFileRef    `json:"python"`
	Entry       contextFileRef    `json:"entry"`
	Manifest    contextFileRef    `json:"manifest"`
	Environment map[string]string `json:"environment"`
}

// A registration grants access to one pre-prepared owner request. It is local
// configuration, never a task checkpoint or a browser-supplied command line.
type contextRegistration struct {
	Schema       int                  `json:"schema"`
	ID           string               `json:"id"`
	Label        string               `json:"label"`
	Request      contextFileRef       `json:"request"`
	Launcher     contextLauncher      `json:"launcher"`
	OriginalHost contextOwnerIdentity `json:"original_host"`
}

type contextChoice struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	Version       string `json:"version"`
	CompositionID string `json:"composition_id"`
	StageID       string `json:"stage_id"`
}

type contextRequestComposition struct {
	Definition contextFileRef `json:"definition"`
	ID         string         `json:"id"`
	StageID    string         `json:"stage_id"`
}

var contextIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$`)
var contextHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func contextRegistryPath(dir string) string { return filepath.Join(dir, "context-controls.json") }
func contextVersion(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func decodeContextJSON(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}
func loadContextRegistrations(dir string) (map[string]contextRegistration, error) {
	data, err := os.ReadFile(contextRegistryPath(dir))
	if os.IsNotExist(err) {
		return map[string]contextRegistration{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, fmt.Errorf("context registration limit exceeded")
	}
	var result map[string]contextRegistration
	if err = decodeContextJSON(data, &result); err != nil {
		return nil, err
	}
	if result == nil || len(result) > 64 {
		return nil, fmt.Errorf("invalid context registration collection")
	}
	for id, item := range result {
		if id != item.ID {
			return nil, fmt.Errorf("context registration identity differs")
		}
	}
	return result, nil
}
func verifyContextRef(ref contextFileRef) error {
	if !filepath.IsAbs(ref.Path) || !contextHashPattern.MatchString(ref.SHA256) {
		return fmt.Errorf("invalid context source reference")
	}
	current, err := loopruntime.ReadSessionReadbackFile(ref.Path, nil)
	if err != nil {
		return err
	}
	if current != ref {
		return fmt.Errorf("registered context source changed: %s", ref.Path)
	}
	return nil
}
func validateContextRegistration(r contextRegistration, identity contextOwnerIdentity) (contextChoice, error) {
	choice := contextChoice{ID: r.ID, Label: r.Label, Version: contextVersion(r)}
	if r.Schema != 1 || !contextIDPattern.MatchString(r.ID) || strings.TrimSpace(r.Label) == "" || len(r.Label) > 200 || r.OriginalHost != identity {
		return choice, fmt.Errorf("context registration identity differs from original Host")
	}
	if err := verifyContextRef(r.Request); err != nil {
		return choice, err
	}
	var request struct {
		Composition *contextRequestComposition `json:"composition"`
	}
	if ref, err := loopruntime.ReadSessionReadbackFile(r.Request.Path, &request); err != nil || ref != r.Request {
		return choice, fmt.Errorf("registered task request changed")
	}
	if request.Composition == nil || request.Composition.ID == "" || request.Composition.StageID == "" {
		return choice, fmt.Errorf("registered task has no fixed composition")
	}
	if err := verifyContextRef(request.Composition.Definition); err != nil {
		return choice, err
	}
	choice.CompositionID, choice.StageID = request.Composition.ID, request.Composition.StageID
	for _, ref := range []contextFileRef{r.Launcher.Python, r.Launcher.Entry, r.Launcher.Manifest} {
		if err := verifyContextRef(ref); err != nil {
			return choice, err
		}
	}
	if err := verifyContextOwnerManifest(r.Launcher); err != nil {
		return choice, err
	}
	if filepath.Base(r.Launcher.Entry.Path) != "soodles.py" {
		return choice, fmt.Errorf("owner entry is not soodles.py")
	}
	info, err := os.Stat(r.Launcher.Python.Path)
	if err != nil {
		return choice, err
	}
	if info.Mode()&0111 == 0 {
		return choice, fmt.Errorf("owner interpreter is not executable")
	}
	for key, value := range r.Launcher.Environment {
		switch key {
		case "HOME", "PATH", "NOODLE_HOST_DIR", "LANG", "LC_ALL", "LC_CTYPE":
		default:
			return choice, fmt.Errorf("owner environment key is not allowed: %s", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return choice, fmt.Errorf("owner environment contains invalid bytes")
		}
	}
	if !filepath.IsAbs(r.Launcher.Environment["HOME"]) || r.Launcher.Environment["PATH"] == "" || r.Launcher.Environment["NOODLE_HOST_DIR"] != identity.HostDir {
		return choice, fmt.Errorf("owner readback environment binding incomplete")
	}
	return choice, nil
}
func readContextHost(ctx context.Context, dir string) (contextOwnerIdentity, error) {
	identity := contextOwnerIdentity{}
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(dir, "host.sock"))
	if err != nil {
		return identity, fmt.Errorf("original Host unavailable: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err = json.NewEncoder(conn).Encode(hostRequest{Operation: "resources"}); err != nil {
		return identity, err
	}
	var response struct {
		InstanceID   string `json:"host_instance_id"`
		BinarySHA256 string `json:"binary_sha256"`
		HostDir      string `json:"host_dir"`
		Shutdown     struct {
			Requested bool `json:"requested"`
		} `json:"shutdown"`
	}
	if err = json.NewDecoder(io.LimitReader(conn, 128<<10)).Decode(&response); err != nil {
		return identity, err
	}
	if response.InstanceID == "" || !contextHashPattern.MatchString(response.BinarySHA256) || response.HostDir != dir || response.Shutdown.Requested {
		return identity, fmt.Errorf("original Host identity unavailable or stopping")
	}
	return contextOwnerIdentity{response.InstanceID, response.BinarySHA256, response.HostDir}, nil
}
func newHostContextsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "contexts", Short: "Register prepared composition controls on the existing Host"}
	var file string
	register := &cobra.Command{Use: "register --file FILE", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		identity, err := readContextHost(cmd.Context(), dir)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if len(data) > 256<<10 {
			return fmt.Errorf("context registration limit exceeded")
		}
		var registration contextRegistration
		if err = decodeContextJSON(data, &registration); err != nil {
			return err
		}
		choice, err := validateContextRegistration(registration, identity)
		if err != nil {
			return err
		}
		lock, err := lockfile.TryLock(filepath.Join(dir, "context-controls.lock"))
		if err != nil {
			return err
		}
		defer lock.Close()
		entries, err := loadContextRegistrations(dir)
		if err != nil {
			return err
		}
		if old, ok := entries[registration.ID]; ok && contextVersion(old) != choice.Version {
			return fmt.Errorf("context registration already exists with different sources; use a new ID")
		}
		entries[registration.ID] = registration
		if len(entries) > 64 {
			return fmt.Errorf("context registration limit exceeded")
		}
		after, err := readContextHost(cmd.Context(), dir)
		if err != nil {
			return err
		}
		if after != identity {
			return fmt.Errorf("original Host changed during registration")
		}
		data, err = json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return err
		}
		if err = filex.WriteFileAtomicDurable(contextRegistryPath(dir), data); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(choice)
	}}
	register.Flags().StringVar(&file, "file", "", "Trusted fixed request, owner source and original Host registration")
	_ = register.MarkFlagRequired("file")
	cmd.AddCommand(register)
	return cmd
}
func contextChoices(entries map[string]contextRegistration, identity contextOwnerIdentity) ([]contextChoice, error) {
	result := make([]contextChoice, 0, len(entries))
	for _, r := range entries {
		if r.OriginalHost != identity {
			continue
		}
		choice, err := validateContextRegistration(r, identity)
		if err != nil {
			return nil, err
		}
		result = append(result, choice)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// Reuse the supervisor runtime manifest format. This verifies a prepared
// source bundle; it neither creates an Issue envelope nor admits a supervisor.
func verifyContextOwnerManifest(launcher contextLauncher) error {
	var manifest struct {
		Schema  int `json:"schema"`
		Runtime []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"runtime"`
	}
	ref, err := loopruntime.ReadSessionReadbackFile(launcher.Manifest.Path, &manifest)
	if err != nil || ref != launcher.Manifest {
		return fmt.Errorf("owner manifest changed")
	}
	if manifest.Schema != 1 || len(manifest.Runtime) == 0 || len(manifest.Runtime) > 512 {
		return fmt.Errorf("owner runtime manifest missing or excessive")
	}
	root := filepath.Dir(launcher.Manifest.Path)
	included := map[string]bool{}
	for _, item := range manifest.Runtime {
		if !filepath.IsLocal(item.Path) || filepath.ToSlash(filepath.Clean(item.Path)) != item.Path || !strings.HasPrefix(item.Path, "runtime/") {
			return fmt.Errorf("owner runtime manifest path is not local runtime source")
		}
		path := filepath.Join(root, filepath.FromSlash(item.Path))
		if included[path] {
			return fmt.Errorf("duplicate owner runtime source")
		}
		if err := verifyContextRef(contextFileRef{Path: path, SHA256: item.SHA256}); err != nil {
			return err
		}
		included[path] = true
	}
	if !included[launcher.Entry.Path] {
		return fmt.Errorf("owner entry is absent from source manifest")
	}
	return filepath.WalkDir(filepath.Join(root, "runtime"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("owner runtime source contains symlink")
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".pyc" {
			return fmt.Errorf("owner runtime contains cached executable bytecode")
		}
		if (ext == ".py" || ext == ".pyc" || ext == ".so" || ext == ".pyd") && !included[path] {
			return fmt.Errorf("owner runtime contains unlisted executable source")
		}
		return nil
	})
}
