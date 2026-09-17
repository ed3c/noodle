package loop

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/orderx"
)

const admissionOwner = "Noodle initial admission"

// AdmissionSubject binds a proposal's original bytes and initial observation.
type AdmissionSubject struct {
	SHA256          string   `json:"proposal_sha256"`
	InitialRevision string   `json:"initial_revision"`
	OrderIDs        []string `json:"order_ids"`
}

type AdmissionNext struct {
	Argv         []string `json:"argv"`
	Guidance     string   `json:"guidance"`
	Required     string   `json:"required"`
	ProvidedBy   string   `json:"provided_by"`
	ReadbackArgv []string `json:"readback_argv"`
}

// AdmissionInspection is the stopped owner's readback, not an admission grant.
type AdmissionInspection struct {
	Owner    string           `json:"owner"`
	Status   string           `json:"status"`
	Subject  AdmissionSubject `json:"subject"`
	Revision string           `json:"current_order_revision"`
	Invalid  string           `json:"invalid"`
	Next     AdmissionNext    `json:"next"`
}

type admissionReceipt struct {
	Owner     string           `json:"owner"`
	Subject   AdmissionSubject `json:"subject"`
	Revision  string           `json:"current_order_revision"`
	Proposal  []byte           `json:"proposal_bytes"`
	Reason    string           `json:"rejection_reason"`
	CreatedAt time.Time        `json:"created_at"`
	Retired   bool             `json:"retired"`
}

// InspectAdmission never creates canonical state or changes a proposal.
func InspectAdmission(projectDir, binary string) AdmissionInspection {
	return recoverAdmission(projectDir, binary, "", "", nil)
}

// RetireAdmission archives an unchanged, rejected initial proposal under the
// same exclusive instance lock used by start. It never dispatches work.
func RetireAdmission(projectDir, binary, digest, revision string) AdmissionInspection {
	if len(digest) != 64 || !orderx.ValidOrderRevision(revision) {
		return admissionRefusal(AdmissionInspection{Owner: admissionOwner, Next: admissionReadback(projectDir, binary)}, fmt.Errorf("invalid proposal digest or current_order_revision"))
	}
	return recoverAdmission(projectDir, binary, digest, revision, nil)
}

func admissionRefusal(r AdmissionInspection, err error) AdmissionInspection {
	r.Status, r.Invalid = "refused", err.Error()
	r.Next.Argv = []string{}
	r.Next.Required = err.Error()
	r.Next.ProvidedBy = "Noodle operator supervising this project and its original session records"
	r.Next.Guidance = "Preserve the reported evidence. The named operator must supply the missing evidence or resolve the reported owner state; then use readback_argv to inspect again. Inspection is not permission to restart or repeat a write."
	return r
}

func recoverAdmission(projectDir, binary, digest, revision string, barrier func(string)) AdmissionInspection {
	r := AdmissionInspection{Owner: admissionOwner, Next: admissionReadback(projectDir, binary)}
	dir := filepath.Join(projectDir, ".noodle")
	lock, err := lockfile.TryLock(filepath.Join(dir, "noodle.lock"))
	if err != nil {
		return admissionRefusal(r, err)
	}
	defer lock.Close()
	snapshot, orders, err := readAdmissionEvidence(dir)
	if err != nil {
		return admissionRefusal(r, err)
	}
	r.Revision = snapshot.OrderRevision
	if revision != "" && revision != r.Revision {
		return admissionRefusal(r, fmt.Errorf("current_order_revision changed"))
	}
	nextPath := filepath.Join(dir, "orders-next.json")
	data, err := readAdmissionFile(nextPath)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return admissionRefusal(r, err)
	}
	// Read the exact receipt before inspecting a potentially later mailbox.
	var receipt *admissionReceipt
	if digest == "" {
		receipt, err = pendingAdmissionReceipt(dir)
		if err != nil {
			return admissionRefusal(r, err)
		}
		if receipt != nil {
			r.Subject = receipt.Subject
			if receipt.Revision != r.Revision {
				return admissionRefusal(r, fmt.Errorf("pending retirement current_order_revision changed"))
			}
			if err := validateRetirementSubject(snapshot, orders, receipt.Proposal); err != nil {
				return admissionRefusal(r, err)
			}
			if !missing && !bytes.Equal(data, receipt.Proposal) {
				return admissionRefusal(r, fmt.Errorf("pending retirement differs from later mailbox; preserve both for owner reconciliation"))
			}
			r.Status, r.Invalid = "recoverable", receipt.Reason
			r.Next = admissionRetirementNext(projectDir, binary, r.Subject.SHA256, r.Revision)
			return r
		}
	}
	if digest != "" {
		receipt, err = readAdmissionReceipt(dir, digest, revision)
		if err != nil {
			return admissionRefusal(r, err)
		}
		if receipt != nil {
			r.Subject = receipt.Subject
			if receipt.Retired || missing {
				if err := validateRetirementSubject(snapshot, orders, receipt.Proposal); err != nil {
					return admissionRefusal(r, err)
				}
				receipt.Retired = true
				if err := persistAdmissionReceipt(dir, *receipt); err != nil {
					return admissionRefusal(r, err)
				}
				return admissionRetired(r, projectDir, binary)
			}
		}
	}
	if missing {
		if digest != "" {
			return admissionRefusal(r, fmt.Errorf("proposal and exact retirement receipt absent"))
		}
		r.Status = "no_proposal"
		r.Next = admissionReadback(projectDir, binary)
		r.Next.Argv = []string{}
		r.Next.Required = "A separately admitted intent and a fresh INITIAL proposal bound to current_order_revision"
		r.Next.ProvidedBy = "Noodle scheduling owner through the existing schedule skill / orders-next.json admission entry"
		r.Next.Guidance = "Retirement is complete. No executable recovery step remains. The scheduling owner may prepare the next proposal from this readback; do not restart a loop or republish a historical proposal from memory."
		return r
	}
	r.Subject.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	if digest != "" && digest != r.Subject.SHA256 {
		return admissionRefusal(r, fmt.Errorf("proposal_sha256 changed; later mailbox preserved"))
	}
	compact, err := orderx.ParseCompactOrders(data)
	if err != nil {
		return admissionRefusal(r, err)
	}
	if compact.InitialRevision == nil || len(compact.Orders) == 0 {
		return admissionRefusal(r, fmt.Errorf("proposal is not a nonempty INITIAL proposal"))
	}
	r.Subject.InitialRevision = *compact.InitialRevision
	r.Subject.OrderIDs = nil
	for _, order := range compact.Orders {
		r.Subject.OrderIDs = append(r.Subject.OrderIDs, order.ID)
	}
	if err := validateRetirementSubject(snapshot, orders, data); err != nil {
		return admissionRefusal(r, err)
	}
	// A valid initial proposal is a legal non-case: leave it for admission.
	if *compact.InitialRevision == r.Revision {
		return admissionRefusal(r, fmt.Errorf("initial proposal is currently valid; retain it for normal admission"))
	}
	reason := fmt.Sprintf("invalid initial_revision=%q: current canonical order_revision=%q", *compact.InitialRevision, r.Revision)
	if receipt == nil {
		receipt, err = readAdmissionReceipt(dir, r.Subject.SHA256, r.Revision)
		if err != nil {
			return admissionRefusal(r, err)
		}
	}
	if receipt != nil && receipt.Retired {
		return admissionRefusal(r, fmt.Errorf("these exact bytes were already retired; mailbox is a later publication"))
	}
	r.Status, r.Invalid = "recoverable", reason
	r.Next = admissionRetirementNext(projectDir, binary, r.Subject.SHA256, r.Revision)
	if digest == "" {
		return r
	}
	if receipt == nil {
		receipt = &admissionReceipt{Owner: admissionOwner, Subject: r.Subject, Revision: r.Revision, Proposal: data, Reason: reason, CreatedAt: time.Now().UTC()}
	}
	if barrier != nil {
		barrier("before_intent")
	}
	if err := persistAdmissionReceipt(dir, *receipt); err != nil {
		return admissionRefusal(r, err)
	}
	if barrier != nil {
		barrier("after_intent")
	}
	// Compare original bytes again after durable evidence, before unlinking.
	current, err := readAdmissionFile(nextPath)
	if err != nil || !bytes.Equal(current, receipt.Proposal) {
		return admissionRefusal(r, fmt.Errorf("mailbox changed after retirement intent: %v", err))
	}
	if err := os.Remove(nextPath); err != nil {
		return admissionRefusal(r, err)
	}
	if err := filex.SyncDir(dir); err != nil {
		return admissionRefusal(r, err)
	}
	if barrier != nil {
		barrier("after_remove")
	}
	receipt.Retired = true
	if err := persistAdmissionReceipt(dir, *receipt); err != nil {
		return admissionRefusal(r, err)
	}
	return admissionRetired(r, projectDir, binary)
}

func admissionReadback(projectDir, binary string) AdmissionNext {
	argv := []string{binary, "--project-dir", projectDir, "admission", "inspect"}
	return AdmissionNext{Argv: argv, ReadbackArgv: argv, Guidance: "Read fresh canonical revision and ownership before preparing a new initial proposal through the existing scheduling/admission flow. Retirement grants no execution or provider authority."}
}

func admissionRetirementNext(projectDir, binary, digest, revision string) AdmissionNext {
	return AdmissionNext{Argv: []string{binary, "--project-dir", projectDir, "admission", "retire", digest, revision}, ReadbackArgv: []string{binary, "--project-dir", projectDir, "admission", "inspect"}, Guidance: "Run this exact argv only for this rejected proposal. Retirement does not admit or restart work."}
}

// Discover durable intent even after mailbox removal, without selecting an
// ambiguous operation by directory order or mutating evidence during inspect.
func pendingAdmissionReceipt(dir string) (*admissionReceipt, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "admission-retirements"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending *admissionReceipt
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		parts := strings.Split(name, "-")
		if len(parts) != 2 || name == entry.Name() || !orderx.ValidOrderRevision(parts[1]) {
			return nil, fmt.Errorf("ambiguous retirement evidence %q", entry.Name())
		}
		receipt, err := readAdmissionReceipt(dir, parts[0], parts[1])
		if err != nil || receipt == nil {
			return nil, fmt.Errorf("read retirement evidence %q: %v", entry.Name(), err)
		}
		if !receipt.Retired {
			if pending != nil {
				return nil, fmt.Errorf("multiple pending retirement intents require owner reconciliation")
			}
			pending = receipt
		}
	}
	return pending, nil
}
func admissionRetired(r AdmissionInspection, projectDir, binary string) AdmissionInspection {
	r.Status, r.Invalid = "retired", ""
	r.Next = admissionReadback(projectDir, binary)
	return r
}

func admissionReceiptPath(dir, digest, revision string) string {
	return filepath.Join(dir, "admission-retirements", digest+"-"+revision+".json")
}
func readAdmissionReceipt(dir, digest, revision string) (*admissionReceipt, error) {
	// Input is also a path component. Reject anything other than the exact hash.
	if len(digest) != 64 {
		return nil, fmt.Errorf("invalid proposal_sha256")
	}
	for _, c := range digest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, fmt.Errorf("invalid proposal_sha256")
		}
	}
	data, err := readAdmissionFile(admissionReceiptPath(dir, digest, revision))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt admissionReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, err
	}
	if receipt.Owner != admissionOwner || receipt.Subject.SHA256 != digest || receipt.Revision != revision || fmt.Sprintf("%x", sha256.Sum256(receipt.Proposal)) != digest || receipt.Reason == "" || receipt.CreatedAt.IsZero() {
		return nil, fmt.Errorf("invalid retirement receipt")
	}
	compact, err := orderx.ParseCompactOrders(receipt.Proposal)
	if err != nil || compact.InitialRevision == nil || *compact.InitialRevision != receipt.Subject.InitialRevision {
		return nil, fmt.Errorf("invalid archived initial proposal")
	}
	ids := make([]string, 0, len(compact.Orders))
	for _, o := range compact.Orders {
		ids = append(ids, o.ID)
	}
	expected, _ := json.Marshal(ids)
	actual, _ := json.Marshal(receipt.Subject.OrderIDs)
	if !bytes.Equal(expected, actual) {
		return nil, fmt.Errorf("retirement subject differs from archived proposal")
	}
	return &receipt, nil
}
func persistAdmissionReceipt(dir string, receipt admissionReceipt) error {
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	path := admissionReceiptPath(dir, receipt.Subject.SHA256, receipt.Revision)
	if err := filex.WriteFileAtomicDurable(path, append(data, '\n')); err != nil {
		return err
	}
	return filex.SyncDir(dir)
}
