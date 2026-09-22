package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/worktree"
)

// StoppedReviewInspection is a non-delivery receipt, never provider authority.
type StoppedReviewInspection struct {
	Owner        string           `json:"owner"`
	Status       string           `json:"status"`
	Claim        PublicationClaim `json:"claim"`
	Digest       string           `json:"custody_sha256"`
	EvidencePath string           `json:"evidence_path"`
	Invalid      string           `json:"invalid"`
	Next         AdmissionNext    `json:"next"`
}

type stoppedReviewIntent struct {
	Claim  PublicationClaim  `json:"claim"`
	Before []byte            `json:"before_snapshot"`
	After  []byte            `json:"after_snapshot"`
	Files  map[string][]byte `json:"session_files"`
	Digest string            `json:"custody_sha256"`
	Bundle string            `json:"bundle_sha256"`
}

func InspectStoppedReview(project, binary, order, subject string) StoppedReviewInspection {
	return recoverStoppedReview(project, binary, order, subject, "", nil)
}

func RejectStoppedReview(project, binary, order, subject, digest string) StoppedReviewInspection {
	if len(digest) != 64 {
		return stoppedReviewRefusal(stoppedReviewResult(project, binary, order, subject), fmt.Errorf("custody_sha256 is not exact"))
	}
	return recoverStoppedReview(project, binary, order, subject, digest, nil)
}

func stoppedReviewResult(project, binary, order, subject string) StoppedReviewInspection {
	read := []string{binary, "--project-dir", project, "review", "inspect", order, subject}
	return StoppedReviewInspection{Owner: "Noodle stopped review", Next: AdmissionNext{Argv: []string{}, ReadbackArgv: read}}
}

func stoppedReviewRefusal(r StoppedReviewInspection, err error) StoppedReviewInspection {
	r.Status, r.Invalid = "refused", err.Error()
	r.Next.Argv = []string{}
	r.Next.Required = err.Error()
	r.Next.ProvidedBy = "Supervisor of the original Noodle order and session"
	r.Next.Guidance = "Preserve evidence and resolve the named condition; then consume fresh readback. Do not restart scheduling or replay historical writes."
	return r
}

func recoverStoppedReview(project, binary, order, subject, digest string, barrier func(string)) StoppedReviewInspection {
	r := stoppedReviewResult(project, binary, order, subject)
	refuse := func(err error) StoppedReviewInspection { return stoppedReviewRefusal(r, err) }
	project, err := canonicalDirectory(project)
	if err != nil {
		return refuse(err)
	}
	dir := filepath.Join(project, ".noodle")
	lock, err := lockfile.TryLock(filepath.Join(dir, "noodle.lock"))
	if err != nil {
		return refuse(err)
	}
	defer lock.Close()
	// A receipt is indexed by the requested subject, never by a caller path.
	r.EvidencePath = filepath.Join(dir, "review-rejections", publicationDigest([]byte(order+"\n"+subject)))
	intentPath := filepath.Join(r.EvidencePath, "intent.json")
	var intent stoppedReviewIntent
	data, err := readAdmissionFile(intentPath)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return refuse(err)
	}
	if exists {
		if err := decodeStoppedReview(data, &intent); err != nil {
			return refuse(err)
		}
		if intent.Claim.OrderID != order || intent.Claim.Subject != subject || intent.Digest != stoppedReviewDigest(intent) {
			return refuse(fmt.Errorf("rejection intent identity or digest differs"))
		}
	} else {
		intent, err = captureStoppedReview(project, order, subject)
		if err != nil {
			return refuse(err)
		}
	}
	r.Claim, r.Digest = intent.Claim, intent.Digest
	if digest != "" && digest != r.Digest {
		return refuse(fmt.Errorf("custody_sha256 changed"))
	}
	current, err := readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		return refuse(err)
	}
	before := bytes.Equal(current, intent.Before)
	after := exists && bytes.Equal(current, intent.After)
	if !before && !after {
		return refuse(fmt.Errorf("canonical snapshot changed outside this exact rejection"))
	}
	if err := validateStoppedSessions(dir, intent, current); err != nil {
		return refuse(err)
	}
	if before {
		fresh, err := captureStoppedReview(project, order, subject)
		if err != nil {
			return refuse(err)
		}
		if fresh.Digest != intent.Digest {
			return refuse(fmt.Errorf("review custody changed"))
		}
	}
	if exists {
		if err := validateStoppedIntent(project, r.EvidencePath, intent); err != nil {
			return refuse(err)
		}
	}
	present, err := stoppedReviewResidue(project, intent.Claim)
	if err != nil {
		return refuse(err)
	}
	if before && !present {
		return refuse(fmt.Errorf("candidate disappeared before canonical rejection"))
	}
	r.Status = "recoverable"
	r.Next.Argv = []string{binary, "--project-dir", project, "review", "reject", order, subject, intent.Digest}
	r.Next.Guidance = "Only an authorized rejection may consume next.argv. Candidate Git objects and original evidence are archived before cleanup. No scheduler or provider operation occurs."
	if after && !present {
		if err := stoppedReviewProjection(dir, intent.After, false); err == nil {
			r.Status = "rejected"
			r.Next.Argv = []string{}
			r.Next.Guidance = "Local Noodle rejection and cleanup are complete. This is not provider delivery or outer-supervisor reconciliation."
			return r
		}
	}
	if digest == "" {
		return r
	}
	if !exists {
		intent.After, err = stoppedReviewAfter(intent.Before, intent.Claim, time.Now().UTC())
		if err != nil {
			return refuse(err)
		}
		if err := archiveStoppedCandidate(project, r.EvidencePath, &intent); err != nil {
			return refuse(err)
		}
		encoded, err := json.MarshalIndent(intent, "", "  ")
		if err != nil {
			return refuse(err)
		}
		if err := filex.WriteFileAtomicDurable(intentPath, append(encoded, '\n')); err != nil {
			return refuse(err)
		}
	}
	if barrier != nil {
		barrier("after_intent")
	}
	// Re-read all custody after archive I/O, before the existing reducer result
	// becomes canonical. No scheduler, signal, or provider client is constructed.
	if before {
		fresh, err := captureStoppedReview(project, order, subject)
		if err != nil {
			return refuse(err)
		}
		if fresh.Digest != intent.Digest {
			return refuse(fmt.Errorf("custody changed after rejection intent"))
		}
		if err := filex.WriteFileAtomicDurable(filepath.Join(dir, "state.snapshot.json"), intent.After); err != nil {
			return refuse(err)
		}
	}
	if barrier != nil {
		barrier("after_canonical")
	}
	if err := stoppedReviewProjection(dir, intent.After, true); err != nil {
		return refuse(err)
	}
	if err := validateStoppedSessions(dir, intent, intent.After); err != nil {
		return refuse(err)
	}
	present, err = stoppedReviewResidue(project, intent.Claim)
	if err != nil {
		return refuse(err)
	}
	if present {
		owner := &worktree.App{Root: project, Quiet: true}
		if err := owner.Cleanup(intent.Claim.WorktreeName, worktree.CleanupOpts{Force: true}); err != nil {
			return refuse(err)
		}
	}
	if barrier != nil {
		barrier("after_cleanup")
	}
	present, err = stoppedReviewResidue(project, intent.Claim)
	if err != nil {
		return refuse(err)
	}
	if present {
		return refuse(fmt.Errorf("cleanup left exact worktree or branch residue; inspect before continuation"))
	}
	r.Status = "rejected"
	r.Next.Argv = []string{}
	r.Next.Guidance = "Local Noodle rejection and cleanup are complete; archived candidate and session evidence remain. No provider delivery or outer-supervisor reconciliation is claimed."
	return r
}

func stoppedReviewAfter(before []byte, claim PublicationClaim, at time.Time) ([]byte, error) {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(before, &s); err != nil {
		return nil, err
	}
	id, err := parseLastEventID(s.State.LastEventID)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"order_id": claim.OrderID, "stage_index": claim.StageIndex, "reason": "supervisor rejected stopped review"})
	next, effects, err := reducer.Reduce(s.State, ingest.StateEvent{ID: ingest.EventID(id + 1), Source: string(ingest.SourceInternal), Type: string(ingest.EventStageReviewRejected), Timestamp: at, Payload: payload})
	if err != nil {
		return nil, err
	}
	if len(effects) != 0 {
		return nil, fmt.Errorf("review rejection unexpectedly requires effects")
	}
	s.State, s.GeneratedAt = next, at
	encoded, err := json.MarshalIndent(s, "", "  ")
	return append(encoded, '\n'), err
}
