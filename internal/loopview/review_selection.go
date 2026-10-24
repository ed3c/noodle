package loopview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

// ReviewSelection is a user's immutable inspection reference, never an execution request.
type ReviewSelection struct {
	Version      int      `json:"version"`
	ID           string   `json:"id"`
	GraphVersion string   `json:"graph_version"`
	RunID        string   `json:"run_id"`
	Kind         string   `json:"kind"`
	TargetID     string   `json:"target_id"`
	Request      Artifact `json:"request"`
	ArtifactIDs  []string `json:"artifact_ids"`
}
type ReviewReadback struct {
	Selection ReviewSelection  `json:"selection"`
	Status    string           `json:"status"`
	Sources   []ArtifactResult `json:"sources"`
	Effects   []string         `json:"effects"`
	Problem   string           `json:"problem,omitempty"`
}

func ReviewVersion(view View) string {
	data, _ := json.Marshal(Metadata(view))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func reviewTarget(view View, run, kind, id string) ([]string, error) {
	if view.Graph == nil {
		return nil, fmt.Errorf("graph missing")
	}
	refs := []string{}
	found := false
	for _, n := range view.Graph.Nodes {
		if n.RunID != run {
			continue
		}
		selected := (kind == "node" && n.ID == id) || (kind == "object" && n.ObjectID == id)
		if selected {
			found = true
			refs = unionRefs(refs, unionRefs(n.InputArtifactIDs, unionRefs(n.OutputArtifactIDs, n.EvidenceArtifactIDs)))
		}
	}
	for _, e := range view.Graph.Edges {
		if e.RunID == run && kind == "edge" && e.ID == id {
			found = true
			refs = unionRefs(refs, unionRefs(e.InputArtifactIDs, unionRefs(e.OutputArtifactIDs, e.EvidenceArtifactIDs)))
		}
	}
	for _, f := range view.Graph.Findings {
		if f.RunID == run && kind == "finding" && f.ID == id {
			found = true
			refs = unionRefs(refs, f.ArtifactIDs)
		}
	}
	if !found {
		return nil, fmt.Errorf("selected target is not in the selected Run")
	}
	return refs, nil
}
func reviewRequest(view View, run string) (Artifact, error) {
	ids := map[string]bool{}
	if view.Graph == nil {
		return Artifact{}, fmt.Errorf("graph missing")
	}
	for _, n := range view.Graph.Nodes {
		if n.RunID == run {
			for _, id := range unionRefs(n.InputArtifactIDs, n.OutputArtifactIDs) {
				ids[id] = true
			}
		}
	}
	var matches []Artifact
	for _, a := range view.Graph.Artifacts {
		if ids[a.ID] && a.Role == "loop_request" {
			matches = append(matches, a)
		}
	}
	if len(matches) != 1 {
		return Artifact{}, fmt.Errorf("Run must have exactly one original loop request")
	}
	a := matches[0]
	if read := ArtifactContent(a); read.Status != "available" || read.Content == nil {
		return Artifact{}, fmt.Errorf("original loop request unavailable or stale")
	}
	a.Content = nil
	a.PublicContent = nil
	return a, nil
}
func reviewID(selection ReviewSelection) string {
	data, _ := json.Marshal([]string{selection.GraphVersion, selection.RunID, selection.Kind, selection.TargetID})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func SaveReviewSelection(dir, version, run, kind, target string) (ReviewReadback, error) {
	lock, err := lockfile.TryLock(filepath.Join(dir, "loop-views.lock"))
	if err != nil {
		return ReviewReadback{}, err
	}
	defer lock.Close()
	view, err := Load(dir)
	if err != nil {
		return ReviewReadback{}, err
	}
	key := reviewID(ReviewSelection{GraphVersion: version, RunID: run, Kind: kind, TargetID: target})
	priorPath := filepath.Join(dir, "loop-review-selections", key+".json")
	if data, readErr := os.ReadFile(priorPath); readErr == nil {
		var prior ReviewSelection
		if json.Unmarshal(data, &prior) != nil || prior.Version != 1 || prior.ID != key || reviewID(prior) != key {
			return ReviewReadback{}, fmt.Errorf("selection receipt changed")
		}
		return readReview(view, prior), nil
	} else if !os.IsNotExist(readErr) {
		return ReviewReadback{}, readErr
	}
	if version != ReviewVersion(view) {
		return ReviewReadback{}, fmt.Errorf("graph version changed; read current graph before selecting")
	}
	refs, err := reviewTarget(view, run, kind, target)
	if err != nil {
		return ReviewReadback{}, err
	}
	request, err := reviewRequest(view, run)
	if err != nil {
		return ReviewReadback{}, err
	}
	refs = unionRefs(refs, []string{request.ID})
	s := ReviewSelection{Version: 1, GraphVersion: version, RunID: run, Kind: kind, TargetID: target, Request: request, ArtifactIDs: refs}
	s.ID = reviewID(s)
	path := filepath.Join(dir, "loop-review-selections", s.ID+".json")
	data, err := json.Marshal(s)
	if err != nil {
		return ReviewReadback{}, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return ReviewReadback{}, err
	}
	prior, err := os.ReadFile(path)
	if err == nil && string(prior) != string(data) {
		return ReviewReadback{}, fmt.Errorf("selection receipt changed")
	}
	if err != nil && !os.IsNotExist(err) {
		return ReviewReadback{}, err
	}
	if os.IsNotExist(err) {
		if err = filex.WriteFileAtomicDurable(path, data); err != nil {
			return ReviewReadback{}, err
		}
	}
	return readReview(view, s), nil
}
func readReview(view View, s ReviewSelection) ReviewReadback {
	result := ReviewReadback{Selection: s, Status: "current", Sources: []ArtifactResult{}, Effects: []string{}}
	if ReviewVersion(view) != s.GraphVersion {
		result.Status = "stale"
	} else {
		refs, targetErr := reviewTarget(view, s.RunID, s.Kind, s.TargetID)
		request, requestErr := reviewRequest(view, s.RunID)
		if targetErr != nil || requestErr != nil || !reflect.DeepEqual(request, s.Request) ||
			!reflect.DeepEqual(unionRefs(refs, []string{request.ID}), s.ArtifactIDs) {
			result.Status = "unknown"
			result.Problem = "saved selection no longer matches original target and request"
		}
	}
	ids := map[string]bool{}
	for _, id := range s.ArtifactIDs {
		ids[id] = true
	}
	if view.Graph != nil {
		for _, a := range view.Graph.Artifacts {
			if ids[a.ID] {
				result.Sources = append(result.Sources, ArtifactContent(a))
				delete(ids, a.ID)
			}
		}
	}
	for _, a := range result.Sources {
		if a.Status != "available" || a.Content == nil {
			if result.Status != "stale" {
				result.Status = "unknown"
			}
			if a.Status == "stale" {
				result.Status = "stale"
				break
			}
		}
	}
	if len(ids) > 0 {
		result.Status = "stale"
	}
	return result
}

// InspectReviewSelections reads explicit references; it neither consumes nor dispatches them.
func InspectReviewSelections(dir, requestPath, id string) ([]ReviewReadback, error) {
	return InspectWorkspaceReviewSelections(dir, "", requestPath, id)
}

// InspectWorkspaceReviewSelections filters by recorded repository, not actor identity.
func InspectWorkspaceReviewSelections(dir, project, requestPath, id string) ([]ReviewReadback, error) {
	view, err := Load(dir)
	if err != nil {
		return nil, err
	}
	if id != "" {
		if decoded, e := hex.DecodeString(id); e != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("invalid review selection id")
		}
	}
	paths, err := filepath.Glob(filepath.Join(dir, "loop-review-selections", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	result := []ReviewReadback{}
	for _, path := range paths {
		if id != "" && filepath.Base(path) != id+".json" {
			continue
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		var s ReviewSelection
		if e = json.Unmarshal(data, &s); e != nil {
			return nil, e
		}
		if s.Version != 1 || s.ID != reviewID(s) || filepath.Base(path) != s.ID+".json" {
			return nil, fmt.Errorf("invalid review selection receipt")
		}
		if project != "" {
			matches := false
			for _, run := range view.Runs {
				if run.ID == s.RunID && run.Repository == project {
					matches = true
				}
			}
			if !matches {
				continue
			}
		}
		if requestPath != "" && (s.Request.Source == nil || s.Request.Source.Path != requestPath) {
			continue
		}
		result = append(result, readReview(view, s))
	}
	if id != "" && len(result) != 1 {
		return nil, fmt.Errorf("selection not found for selected workspace or request")
	}
	return result, nil
}
