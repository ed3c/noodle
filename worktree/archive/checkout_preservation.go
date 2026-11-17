package archive

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/poteto/noodle/internal/lockfile"
)

type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// CheckoutIdentity has the existing worktree.GCCustodyIdentity wire shape.
// The worktree adapter checks it through the original custody reader as well.
type CheckoutIdentity struct {
	Repo         string `json:"repo"`
	Path         string `json:"path"`
	Registration string `json:"registration"`
	Incarnation  string `json:"incarnation"`
	Head         string `json:"head"`
	Ref          string `json:"ref"`
}

type CheckoutDisposition struct {
	Path        string `json:"path"`
	Owner       string `json:"owner"`
	Disposition string `json:"disposition"`
	SHA256      string `json:"sha256"`
}

type CheckoutRequest struct {
	Operation string                `json:"operation"`
	Identity  CheckoutIdentity      `json:"identity"`
	Inventory FileRef               `json:"inventory"`
	Config    FileRef               `json:"config"`
	Ignored   []CheckoutDisposition `json:"ignored"`
}

type TestRequestDisposition struct {
	RequestID   string   `json:"request_id"`
	State       string   `json:"state"`
	Disposition string   `json:"disposition"`
	Gaps        []string `json:"gaps"`
	Receipt     *FileRef `json:"receipt"`
}

type TestInventory struct {
	Owner  string `json:"owner"`
	Status string `json:"status"`
	Origin struct {
		Repository   string    `json:"repository"`
		GitDir       string    `json:"git_dir"`
		CommonDir    string    `json:"common_dir"`
		BindingFiles []FileRef `json:"binding_files"`
	} `json:"origin"`
	Namespace            string                   `json:"namespace"`
	NamespaceObservation string                   `json:"namespace_observation"`
	Files                []FileRef                `json:"files"`
	Requests             []TestRequestDisposition `json:"requests"`
	Gaps                 []string                 `json:"gaps"`
	Disposition          string                   `json:"disposition"`
	AuthorizesGC         bool                     `json:"authorizes_gc"`
	AuthorizesLanding    bool                     `json:"authorizes_landing"`
	RegistrationComplete bool                     `json:"registration_complete"`
	PendingOwners        []string                 `json:"pending_owners"`
	Consistency          string                   `json:"consistency"`
	Effects              []json.RawMessage        `json:"effects"`
	Next                 json.RawMessage          `json:"next"`
}

type CheckoutPacket struct {
	Schema                int             `json:"schema"`
	Request               CheckoutRequest `json:"request"`
	Source                Source          `json:"source"`
	Tree                  Tree            `json:"checkout"`
	CheckoutLink          Entry           `json:"checkout_git_link"`
	OwnedBindingRoot      Metadata        `json:"owned_binding_root"`
	Pool                  string          `json:"git_pool"`
	CandidateRef          string          `json:"candidate_ref"`
	Manifest              FileRef         `json:"manifest"`
	InventoryCopy         FileRef         `json:"inventory_copy"`
	Recovery              string          `json:"recovery"`
	RegistrationRecovery  string          `json:"registration_recovery"`
	VerificationCopy      Tree            `json:"verification_copy"`
	VerificationIdentity  string          `json:"verification_identity"`
	Phase                 string          `json:"phase"`
	AuthorizesGC          bool            `json:"authorizes_gc"`
	RegistrationInstalled bool            `json:"registration_installed"`
	Signature             string          `json:"signature"`
}

func DecodeCheckoutJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("JSON has trailing data")
	}
	return nil
}

func readRef(ref FileRef) ([]byte, error) {
	if !validHash(ref.SHA256) {
		return nil, errors.New("FileRef hash is invalid")
	}
	if err := canonical(ref.Path); err != nil {
		return nil, err
	}
	st, err := os.Lstat(ref.Path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("FileRef is not regular: %s", ref.Path)
	}
	b, err := os.ReadFile(ref.Path)
	if err != nil {
		return nil, err
	}
	if rootKey(string(b)) != ref.SHA256 {
		return nil, fmt.Errorf("FileRef hash differs: %s", ref.Path)
	}
	return b, nil
}

func checkoutConfig(r CheckoutRequest) (Config, error) {
	var c Config
	if strings.TrimSpace(r.Operation) == "" {
		return c, errors.New("operation is missing")
	}
	checked, err := readRef(r.Config)
	if err != nil {
		return c, err
	}
	c, err = parseConfig(checked)
	if err != nil {
		return c, err
	}
	for _, p := range []string{c.ArchiveRoot, c.BlobRoot, c.StateDir, r.Config.Path, r.Inventory.Path} {
		for _, source := range []string{r.Identity.Path, r.Identity.Registration, r.Identity.Repo} {
			if within(source, p) || within(p, source) {
				return c, fmt.Errorf("external storage/input overlaps source: %s", p)
			}
		}
	}
	if !c.Managed(r.Identity.Path) {
		return c, errors.New("checkout is outside configured scope")
	}
	return c, nil
}

func inspectCheckout(c Config, r CheckoutRequest) (Source, error) {
	for _, root := range c.ExternalOwnerRoots {
		if within(root, r.Identity.Path) || within(r.Identity.Path, root) {
			return Source{}, fmt.Errorf("external Owner disposition unavailable HOLD: %s", root)
		}
	}
	s, err := InspectSource(c, r.Identity.Path)
	if err != nil {
		return s, err
	}
	id := r.Identity
	if s.Common != id.Repo || s.Admin != id.Registration || s.Head != id.Head || s.Branch != id.Ref || !strings.HasPrefix(id.Ref, "refs/heads/") || filepath.Dir(s.Admin) != filepath.Join(s.Common, "worktrees") {
		return s, errors.New("checkout Git identity/ref differs")
	}
	var token string
	b, err := os.ReadFile(filepath.Join(s.Admin, "noodle-incarnation.json"))
	if err != nil {
		return s, err
	}
	if err := DecodeCheckoutJSON(b, &token); err != nil || !validHash(token) || token != id.Incarnation {
		return s, errors.New("checkout incarnation differs")
	}
	commonLink, err := os.ReadFile(filepath.Join(s.Admin, "commondir"))
	if err != nil {
		return s, err
	}
	if filepath.Clean(filepath.Join(s.Admin, strings.TrimSpace(string(commonLink)))) != s.Common {
		return s, errors.New("registration commondir differs")
	}
	for p, e := range s.AdminTree.Entries {
		if e.Kind == "link" {
			return s, fmt.Errorf("registration symlink requires HOLD: %s", p)
		}
	}
	return s, nil
}

func checkInventory(r CheckoutRequest, s Source) ([]byte, error) {
	if r.Inventory == (FileRef{}) {
		return observeForeignNamespaceAbsence(s)
	}
	raw, err := readRef(r.Inventory)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := DecodeCheckoutJSON(raw, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"owner", "status", "origin", "namespace", "namespace_observation", "files", "requests", "gaps", "disposition", "authorizes_gc", "authorizes_landing", "registration_complete", "pending_owners", "consistency", "effects", "next"} {
		if _, ok := fields[name]; !ok {
			return nil, fmt.Errorf("original Test inventory field missing: %s", name)
		}
	}
	var inv TestInventory
	if err := DecodeCheckoutJSON(raw, &inv); err != nil {
		return nil, fmt.Errorf("original Test inventory schema: %w", err)
	}
	ns := "soodles-test-requests.noindex"
	if (inv.Disposition != "PRESERVE" && inv.Disposition != "OBSERVED_ABSENCE") || inv.Owner != "test-manager" || inv.Status != "observed" || inv.AuthorizesGC || inv.AuthorizesLanding || inv.RegistrationComplete || len(inv.Gaps) != 0 || len(inv.Effects) != 0 || string(inv.Next) != "null" {
		return nil, errors.New("Test inventory HOLD or incompatible snapshot")
	}
	if inv.Origin.Repository != s.Path || inv.Origin.GitDir != s.Admin || inv.Origin.CommonDir != s.Common || inv.Namespace != filepath.Join(s.Admin, ns) {
		return nil, errors.New("Test inventory origin differs")
	}
	wantBindings := map[string]bool{filepath.Join(s.Path, ".git"): true, filepath.Join(s.Admin, "gitdir"): true, filepath.Join(s.Admin, "commondir"): true}
	for _, ref := range inv.Origin.BindingFiles {
		if !wantBindings[ref.Path] {
			return nil, errors.New("Test inventory foreign/duplicate binding")
		}
		if _, err := readRef(ref); err != nil {
			return nil, err
		}
		delete(wantBindings, ref.Path)
	}
	if len(wantBindings) != 0 {
		return nil, errors.New("Test inventory binding incomplete")
	}
	files := map[string]string{}
	dirs := map[string]bool{ns: true}
	for _, ref := range inv.Files {
		rel, err := filepath.Rel(s.Admin, ref.Path)
		if err != nil || !safeRelative(rel) || !strings.HasPrefix(rel, ns+"/") || !validHash(ref.SHA256) || files[rel] != "" {
			return nil, errors.New("Test inventory foreign/duplicate file")
		}
		e, ok := s.AdminTree.Entries[rel]
		if !ok || e.Kind != "file" || e.Hash != ref.SHA256 {
			return nil, fmt.Errorf("Test inventory file missing or hash drift: %s", rel)
		}
		files[rel] = ref.SHA256
		for p := filepath.Dir(rel); p != ns; p = filepath.Dir(p) {
			dirs[p] = true
		}
	}
	seenRequests := map[string]bool{}
	for _, row := range inv.Requests {
		if seenRequests[row.RequestID] {
			return nil, errors.New("Test request duplicate")
		}
		seenRequests[row.RequestID] = true
		if !validHash(row.RequestID) || row.Disposition != "PRESERVE" || len(row.Gaps) != 0 || (row.State != "completed" && row.State != "failed" && row.State != "refused") || row.Receipt == nil {
			return nil, errors.New("Test request nonterminal/UNKNOWN or incomplete")
		}
		if row.Receipt.Path != filepath.Join(s.Admin, ns, row.RequestID, "receipt.json") || files[ns+"/"+row.RequestID+"/receipt.json"] != row.Receipt.SHA256 {
			return nil, errors.New("Test receipt binding differs")
		}
	}
	count := 0
	for rel, e := range s.AdminTree.Entries {
		if rel != ns && !strings.HasPrefix(rel, ns+"/") {
			continue
		}
		count++
		if rel != ns {
			parts := strings.Split(strings.TrimPrefix(rel, ns+"/"), "/")
			if (len(parts) > 1 || e.Kind == "dir") && !seenRequests[parts[0]] {
				return nil, fmt.Errorf("Test inventory request disposition missing: %s", rel)
			}
		}
		if e.Kind == "dir" && dirs[rel] {
			continue
		}
		if e.Kind != "file" || files[rel] != e.Hash {
			return nil, fmt.Errorf("Test inventory extra/unclassified entry: %s", rel)
		}
	}
	switch inv.NamespaceObservation {
	case "observed_absent":
		if inv.Disposition != "OBSERVED_ABSENCE" || count != 0 || len(inv.Files) != 0 || len(inv.Requests) != 0 {
			return nil, errors.New("Test namespace absence differs")
		}
	case "observed_empty":
		if inv.Disposition != "PRESERVE" || count != 1 || len(inv.Files) != 0 || len(inv.Requests) != 0 {
			return nil, errors.New("Test namespace empty observation differs")
		}
	case "observed_present":
		if inv.Disposition != "PRESERVE" || len(inv.Files) == 0 || len(inv.Requests) == 0 {
			return nil, errors.New("Test namespace present observation incomplete")
		}
	default:
		return nil, errors.New("Test namespace UNKNOWN")
	}
	return raw, nil
}

func checkCheckoutDisposition(r CheckoutRequest, s Source, t Tree) error {
	known := map[string]CheckoutDisposition{}
	for _, d := range r.Ignored {
		if !safeRelative(d.Path) || d.Owner != "worktree" || d.Disposition != "PRESERVE" {
			return fmt.Errorf("unknown checkout Owner/disposition: %s", d.Path)
		}
		if _, ok := known[d.Path]; ok {
			return errors.New("duplicate checkout disposition")
		}
		known[d.Path] = d
	}
	for rel, e := range t.Entries {
		if e.Kind == "link" {
			return fmt.Errorf("checkout symlink HOLD: %s", rel)
		}
		if s.Tracked[rel] {
			continue
		}
		if e.Kind == "dir" {
			trackedParent := false
			for p := range s.Tracked {
				if strings.HasPrefix(p, rel+"/") {
					trackedParent = true
					break
				}
			}
			if trackedParent {
				continue
			}
		}
		d, ok := known[rel]
		if !ok {
			return fmt.Errorf("unclassified checkout content HOLD: %s", rel)
		}
		if d.SHA256 != e.Hash {
			return fmt.Errorf("checkout disposition hash differs: %s", rel)
		}
		if e.Kind == "file" {
			f, err := os.Open(filepath.Join(s.Path, rel))
			if err != nil {
				return err
			}
			var magic [16]byte
			_, readErr := f.Read(magic[:])
			closeErr := f.Close()
			if readErr != nil && readErr != io.EOF {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			if string(magic[:]) == "SQLite format 3\x00" || strings.HasSuffix(rel, "-wal") || strings.HasSuffix(rel, "-shm") {
				return fmt.Errorf("SQLite Owner/quiescence unknown HOLD: %s", rel)
			}
		}
		delete(known, rel)
	}
	if len(known) != 0 {
		return errors.New("checkout disposition contains foreign/absent/tracked entries")
	}
	return nil
}

func checkoutLink(root string) (Entry, error) {
	p := filepath.Join(root, ".git")
	st, err := os.Lstat(p)
	if err != nil {
		return Entry{}, err
	}
	if !st.Mode().IsRegular() {
		return Entry{}, errors.New("checkout Git link is nonregular")
	}
	if err := CheckIndependentFile(p); err != nil {
		return Entry{}, err
	}
	m, err := ReadMetadata(p)
	if err != nil {
		return Entry{}, err
	}
	hash, err := FileHash(p)
	return Entry{Kind: "file", Mode: uint32(st.Mode().Perm()), Size: st.Size(), MtimeNS: st.ModTime().UnixNano(), Hash: hash, Metadata: m, HasMetadata: true}, err
}

func packetPath(c Config, operation string) string {
	return filepath.Join(c.ArchiveRoot, "checkout-preservation", rootKey(operation)+".json")
}

func sealPacket(c Config, p CheckoutPacket) (CheckoutPacket, error) {
	p.Signature = ""
	signature, err := authenticate(c, p)
	p.Signature = signature
	return p, err
}

func publishImmutable(path string, value any) error {
	// Publish with a link so concurrent producers cannot replace the first result.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".preservation-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	raw, err := json.Marshal(value)
	if err == nil {
		_, err = tmp.Write(append(raw, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return err
	}
	if err = os.Link(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// PrepareCheckout retains every partial result. Retrying a partial operation
// requires original Owner reconciliation, never a new operation identifier.
func PrepareCheckout(r CheckoutRequest) (CheckoutPacket, error) {
	var p CheckoutPacket
	c, err := checkoutConfig(r)
	if err != nil {
		return p, err
	}
	path := packetPath(c, r.Operation)
	if raw, err := os.ReadFile(path); err == nil {
		if err = DecodeCheckoutJSON(raw, &p); err != nil {
			return p, err
		}
		if !reflect.DeepEqual(p.Request, r) {
			return p, errors.New("operation conflict; inspect original packet")
		}
		return InspectCheckout(p)
	} else if !os.IsNotExist(err) {
		return p, err
	}
	s, err := inspectCheckout(c, r)
	if err != nil {
		return p, err
	}
	inv, err := checkInventory(r, s)
	if err != nil {
		return p, err
	}
	t, err := Capture(s.Path, s.Excluded)
	if err != nil {
		return p, err
	}
	if err = checkCheckoutDisposition(r, s, t); err != nil {
		return p, err
	}
	link, err := checkoutLink(s.Path)
	if err != nil {
		return p, err
	}
	dir := filepath.Dir(path)
	if err = durableMkdirAll(dir, 0700); err != nil {
		return p, err
	}
	// The reservation is permanent even on failure. It protects partial CAS/pool
	// state from being mistaken for an absent operation.
	sourceReservation := filepath.Join(dir, "source-"+rootKey(r.Identity.Path+"\x00"+r.Identity.Incarnation)+".reservation")
	if err = publishImmutable(sourceReservation, r); err != nil {
		return p, fmt.Errorf("source already reserved; original operation readback/reconciliation required: %w", err)
	}
	reservation := path + ".reservation"
	if err = publishImmutable(reservation, r); err != nil {
		return p, fmt.Errorf("partial/conflicting operation; source and external data retained: %w", err)
	}
	id := rootKey(r.Operation)
	p = CheckoutPacket{Schema: 1, Request: r, Source: s, Tree: t, CheckoutLink: link, CandidateRef: "refs/archive/" + id, Phase: "stored"}
	p.Manifest.Path = filepath.Join(dir, id+".manifest.json")
	entries, err := checkoutManifest(p)
	if err != nil {
		return p, err
	}
	if err = publishImmutable(p.Manifest.Path, entries); err != nil {
		return p, err
	}
	p.Manifest.SHA256, err = FileHash(p.Manifest.Path)
	if err != nil {
		return p, err
	}
	p.InventoryCopy.Path = filepath.Join(dir, id+".inventory.json")
	// Preserve the exact original Consumer bytes, including its formatting.
	if err = writeImmutableBytes(p.InventoryCopy.Path, inv); err != nil {
		return p, err
	}
	p.InventoryCopy.SHA256 = rootKey(string(inv))
	if p.Pool, err = ensurePool(c, s, id); err != nil {
		return p, err
	}
	store := Store{Root: c.BlobRoot, Codec: c.Codec}
	for rel, e := range t.Entries {
		if e.Kind == "file" && !s.Tracked[rel] {
			if err = store.Put(filepath.Join(s.Path, rel), e); err != nil {
				return p, err
			}
		}
	}
	for rel, e := range s.AdminTree.Entries {
		if e.Kind == "file" {
			if err = store.Put(filepath.Join(s.Admin, rel), e); err != nil {
				return p, err
			}
		}
	}
	if err = store.Put(filepath.Join(s.Path, ".git"), link); err != nil {
		return p, err
	}
	recovery := filepath.Join(dir, id+".recovery")
	if err = durableMkdirAll(recovery, 0700); err != nil {
		return p, err
	}
	p.Recovery = filepath.Join(recovery, "checkout")
	p.RegistrationRecovery = filepath.Join(recovery, "registration")
	if err = restoreCheckout(c, &p); err != nil {
		return p, fmt.Errorf("independent restore failed; all source/external bytes retained, operation UNKNOWN: %w", err)
	}
	if p.VerificationCopy, err = capture(filepath.Dir(p.Recovery), nil, true); err != nil {
		return p, err
	}
	if p.VerificationIdentity, err = FileIdentity(filepath.Dir(p.Recovery)); err != nil {
		return p, err
	}
	if err = verifySource(c, p); err != nil {
		return p, err
	}
	p.Phase = "verified"
	p, err = sealPacket(c, p)
	if err != nil {
		return p, err
	}
	if err = publishImmutable(path, p); err != nil {
		return p, fmt.Errorf("publication UNKNOWN; inspect original operation: %w", err)
	}
	return InspectCheckout(p)
}

func writeImmutableBytes(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close(), syncDir(filepath.Dir(path)))
}

func verifySource(c Config, p CheckoutPacket) error {
	s, err := inspectCheckout(c, p.Request)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(s, p.Source) {
		return errors.New("source Git/registration/index drift; source retained")
	}
	if _, err = checkInventory(p.Request, s); err != nil {
		return err
	}
	if err = VerifyTree(s.Path, p.Tree); err != nil {
		return fmt.Errorf("checkout Source drift: %w", err)
	}
	link, err := checkoutLink(s.Path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(link, p.CheckoutLink) {
		return errors.New("checkout Git link drift")
	}
	return nil
}

// The CAS manifest describes content only. Per-path modes, timestamps and
// extended metadata remain in the checkout and registration Trees.
func checkoutManifest(p CheckoutPacket) (map[string]Entry, error) {
	entries := map[string]Entry{}
	add := func(e Entry) error {
		if e.Kind != "file" || !validHash(e.Hash) || e.Size < 0 {
			return errors.New("CAS content descriptor is invalid")
		}
		if prior, ok := entries[e.Hash]; ok && prior.Size != e.Size {
			return fmt.Errorf("CAS content size conflict: %s", e.Hash)
		}
		entries[e.Hash] = Entry{Kind: "file", Hash: e.Hash, Size: e.Size, HasMetadata: true, Metadata: Metadata{Xattrs: map[string]string{}}}
		return nil
	}
	for rel, e := range p.Tree.Entries {
		if e.Kind == "file" && !p.Source.Tracked[rel] {
			if err := add(e); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range p.Source.AdminTree.Entries {
		if e.Kind == "file" {
			if err := add(e); err != nil {
				return nil, err
			}
		}
	}
	if err := add(p.CheckoutLink); err != nil {
		return nil, err
	}
	return entries, nil
}

func linkTree(p CheckoutPacket) Tree {
	return Tree{Entries: map[string]Entry{"checkout.git": p.CheckoutLink}, Root: p.OwnedBindingRoot, Mode: 0700}
}

func restoreCheckout(c Config, p *CheckoutPacket) error {
	if _, err := git(filepath.Dir(p.Recovery), "clone", "--no-local", "--no-checkout", "--", p.Pool, p.Recovery); err != nil {
		return err
	}
	if _, err := git(p.Recovery, "fetch", "--no-auto-maintenance", "--no-write-fetch-head", p.Pool, p.CandidateRef+":refs/archive/recovered"); err != nil {
		return err
	}
	if _, err := git(p.Recovery, "checkout", "--detach", p.Source.Head); err != nil {
		return err
	}
	store := Store{Root: c.BlobRoot, Codec: c.Codec}
	if err := store.Materialize(p.Recovery, p.Tree, p.Source.Tracked); err != nil {
		return err
	}
	if err := os.Mkdir(p.RegistrationRecovery, 0700); err != nil {
		return err
	}
	if err := store.Materialize(p.RegistrationRecovery, p.Source.AdminTree, map[string]bool{}); err != nil {
		return err
	}
	binding := filepath.Join(filepath.Dir(p.Recovery), "binding")
	if err := os.Mkdir(binding, 0700); err != nil {
		return err
	}
	root, err := ReadMetadata(binding)
	if err != nil {
		return err
	}
	p.OwnedBindingRoot = root
	if err := store.Materialize(binding, linkTree(*p), map[string]bool{}); err != nil {
		return err
	}
	return verifyRecovery(c, *p)
}

func verifyRecovery(c Config, p CheckoutPacket) error {
	for _, path := range []string{p.Pool, p.Recovery, p.RegistrationRecovery} {
		if err := canonical(path); err != nil {
			return err
		}
	}
	for _, repo := range []string{p.Pool, p.Recovery} {
		objectRoot := filepath.Join(repo, "objects")
		if repo == p.Recovery {
			objectRoot = filepath.Join(repo, ".git", "objects")
		}
		if err := filepath.WalkDir(objectRoot, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("Git object storage symlink HOLD: %s", path)
			}
			return nil
		}); err != nil {
			return err
		}
		objects := filepath.Join(repo, "objects", "info", "alternates")
		if repo == p.Recovery {
			objects = filepath.Join(repo, ".git", "objects", "info", "alternates")
		}
		if _, err := os.Lstat(objects); !os.IsNotExist(err) {
			return errors.New("archive/restored objects borrowed or unknown")
		}
		if _, err := git(repo, "fsck", "--full", "--no-reflogs", p.Source.Head); err != nil {
			return err
		}
	}
	ref, err := git(p.Pool, "rev-parse", "--verify", p.CandidateRef)
	if err != nil {
		return fmt.Errorf("durable candidate ref unavailable: %w", err)
	}
	if strings.TrimSpace(ref) != p.Source.Head {
		return errors.New("durable candidate ref differs")
	}
	head, err := git(p.Recovery, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != p.Source.Head {
		return errors.New("recovered HEAD differs")
	}
	status, err := git(p.Recovery, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("restored Git index/status differs")
	}
	if err = VerifyTree(p.Recovery, p.Tree); err != nil {
		return err
	}
	if err = VerifyTree(p.RegistrationRecovery, p.Source.AdminTree); err != nil {
		return err
	}
	if err = VerifyTree(filepath.Join(filepath.Dir(p.Recovery), "binding"), linkTree(p)); err != nil {
		return err
	}
	if len(p.VerificationCopy.Entries) != 0 {
		identity, err := FileIdentity(filepath.Dir(p.Recovery))
		if err != nil || identity != p.VerificationIdentity {
			return fmt.Errorf("verification copy directory identity changed: %v", err)
		}
		actual, err := capture(filepath.Dir(p.Recovery), nil, true)
		if err != nil || !reflect.DeepEqual(actual, p.VerificationCopy) {
			return fmt.Errorf("verification copy identity/content changed; retain copy: %v", err)
		}
	}
	return nil
}

// inspectStoredCheckout validates external evidence without reading the source.
func inspectStoredCheckout(p CheckoutPacket) (CheckoutPacket, error) {
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return p, err
	}
	if p.Schema != 1 || p.Phase != "verified" || p.AuthorizesGC || p.RegistrationInstalled {
		return p, errors.New("packet incomplete/UNKNOWN or incompatible authority")
	}
	sealed, err := sealPacket(c, p)
	if err != nil {
		return p, err
	}
	if !hmac.Equal([]byte(sealed.Signature), []byte(p.Signature)) {
		return p, errors.New("packet signature differs")
	}
	if err := canonical(packetPath(c, p.Request.Operation)); err != nil {
		return p, err
	}
	raw, err := os.ReadFile(packetPath(c, p.Request.Operation))
	if err != nil {
		return p, err
	}
	var original CheckoutPacket
	if err = DecodeCheckoutJSON(raw, &original); err != nil {
		return p, err
	}
	if !reflect.DeepEqual(original, p) {
		return p, errors.New("immutable packet readback differs")
	}
	id := rootKey(p.Request.Operation)
	dir := filepath.Dir(packetPath(c, p.Request.Operation))
	for _, reservation := range []string{packetPath(c, p.Request.Operation) + ".reservation", filepath.Join(dir, "source-"+rootKey(p.Request.Identity.Path+"\x00"+p.Request.Identity.Incarnation)+".reservation")} {
		if err := canonical(reservation); err != nil {
			return p, err
		}
		raw, err := os.ReadFile(reservation)
		if err != nil {
			return p, err
		}
		var r CheckoutRequest
		if err = DecodeCheckoutJSON(raw, &r); err != nil {
			return p, err
		}
		if !reflect.DeepEqual(r, p.Request) {
			return p, errors.New("operation reservation conflict")
		}
	}

	if p.Pool != filepath.Join(c.ArchiveRoot, "git-pools", rootKey(p.Source.Common)+".git") || p.CandidateRef != "refs/archive/"+id || p.Manifest.Path != filepath.Join(dir, id+".manifest.json") || p.InventoryCopy.Path != filepath.Join(dir, id+".inventory.json") || p.Recovery != filepath.Join(dir, id+".recovery", "checkout") || p.RegistrationRecovery != filepath.Join(dir, id+".recovery", "registration") {
		return p, errors.New("packet storage paths differ")
	}
	if _, err = readRef(p.InventoryCopy); err != nil {
		return p, err
	}
	if p.Request.Inventory != (FileRef{}) && p.InventoryCopy.SHA256 != p.Request.Inventory.SHA256 {
		return p, errors.New("inventory copy differs")
	}
	if _, err = readRef(p.Manifest); err != nil {
		return p, err
	}
	entries, err := ReadManifest(p.Manifest.Path, c.Codec)
	if err != nil {
		return p, err
	}
	expected, err := checkoutManifest(p)
	if err != nil {
		return p, err
	}
	if !reflect.DeepEqual(entries, expected) {
		return p, errors.New("CAS manifest inventory differs")
	}
	store := Store{Root: c.BlobRoot, Codec: c.Codec}
	for hash, e := range entries {
		if err = store.Verify(hash, e.Size); err != nil {
			return p, err
		}
	}
	return p, nil
}

// InspectCheckout validates both original source and external recovery read-only.
func InspectCheckout(p CheckoutPacket) (CheckoutPacket, error) {
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return p, err
	}
	lock, err := lockfile.TryLock(packetPath(c, p.Request.Operation) + ".release.lock")
	if err != nil {
		return p, fmt.Errorf("verification copy has another owner consumer: %w", err)
	}
	defer lock.Close()
	p, err = inspectStoredCheckout(p)
	if err != nil {
		return p, err
	}
	if err = verifySource(c, p); err != nil {
		return p, err
	}
	if err = verifyRecovery(c, p); err != nil {
		return p, err
	}
	return p, nil
}

// RestoreCheckoutRecovery creates a fresh standalone checkout and a separate
// registration representation. Original Git Owner installation is a later act.
func RestoreCheckoutRecovery(p CheckoutPacket, destination string) error {
	if _, err := inspectStoredCheckout(p); err != nil {
		return err
	}
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return errors.New("recovery destination is not absolute and clean")
	}
	if err = canonical(filepath.Dir(destination)); err != nil {
		return err
	}
	for _, root := range []string{p.Request.Identity.Path, p.Request.Identity.Repo, p.Request.Identity.Registration, c.ArchiveRoot, c.BlobRoot, c.StateDir} {
		if within(root, destination) || within(destination, root) {
			return errors.New("recovery destination overlaps protected source/storage")
		}
	}
	if err = os.Mkdir(destination, 0700); err != nil {
		return err
	}
	p.Recovery = filepath.Join(destination, "checkout")
	p.RegistrationRecovery = filepath.Join(destination, "registration")
	p.VerificationCopy = Tree{}
	p.VerificationIdentity = ""
	if err = restoreCheckout(c, &p); err != nil {
		return fmt.Errorf("recovery failed; source and partial recovery retained: %w", err)
	}
	return nil
}
