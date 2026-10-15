package loopview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"strings"
)

type Graph struct {
	Objects      []Object     `json:"objects"`
	Ports        []Port       `json:"ports"`
	Repositories []Repository `json:"repositories"`
	Nodes        []Node       `json:"nodes"`
	Artifacts    []Artifact   `json:"artifacts"`
	Edges        []Edge       `json:"edges"`
	Findings     []Finding    `json:"findings"`
	Metrics      []Metric     `json:"metrics"`
}
type Object struct {
	ID                string   `json:"id"`
	Namespace         string   `json:"namespace"`
	Label             string   `json:"label"`
	Kind              string   `json:"kind"`
	Scope             string   `json:"scope"`
	SourceArtifactIDs []string `json:"source_artifact_ids"`
}
type Port struct {
	ID                string   `json:"id"`
	ObjectID          string   `json:"object_id"`
	Name              string   `json:"name"`
	Direction         string   `json:"direction"`
	Schema            string   `json:"schema"`
	EffectScope       string   `json:"effect_scope"`
	SourceArtifactIDs []string `json:"source_artifact_ids"`
}
type Repository struct {
	ID            string      `json:"id"`
	Label         string      `json:"label"`
	Root          string      `json:"root"`
	Revision      *string     `json:"revision"`
	IndexStatus   string      `json:"index_status"`
	TrackedFiles  []IndexFile `json:"tracked_files"`
	SelectedFiles []IndexFile `json:"selected_files"`
	Gaps          []string    `json:"gaps"`
}
type IndexFile struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	GitBlob     *string  `json:"git_blob"`
	GitMode     *string  `json:"git_mode"`
	Bytes       *int64   `json:"bytes"`
	ArtifactIDs []string `json:"artifact_ids"`
	NodeIDs     []string `json:"node_ids"`
}
type ArtifactSource struct {
	Path   string  `json:"path"`
	SHA256 *string `json:"sha256"`
	Bytes  *int64  `json:"bytes"`
}
type Artifact struct {
	ID              string          `json:"id"`
	RepositoryID    *string         `json:"repository_id"`
	Role            string          `json:"role"`
	Format          string          `json:"format"`
	Authority       string          `json:"authority"`
	KnowledgeRole   string          `json:"knowledge_role"`
	Scope           string          `json:"scope"`
	Source          *ArtifactSource `json:"source"`
	Status          string          `json:"status"`
	Content         *string         `json:"content,omitempty"`
	ContentEncoding string          `json:"content_encoding,omitempty"`
	PublicContent   *string         `json:"public_content,omitempty"`
	PublicSHA256    *string         `json:"public_sha256,omitempty"`
	PublicBytes     *int64          `json:"public_bytes,omitempty"`
}
type NodeIdentity struct {
	Status       string  `json:"status"`
	InvocationID *string `json:"invocation_id"`
	AttemptID    *string `json:"attempt_id"`
	SessionID    *string `json:"session_id"`
	Locator      *string `json:"locator"`
}
type NodeStatus struct {
	Execution  string `json:"execution"`
	Transfer   string `json:"transfer"`
	Acceptance string `json:"acceptance"`
}
type Node struct {
	ID                  string       `json:"id"`
	RunID               string       `json:"run_id"`
	RepositoryID        string       `json:"repository_id"`
	ObjectID            string       `json:"object_id"`
	Kind                string       `json:"kind"`
	Label               string       `json:"label"`
	Identity            NodeIdentity `json:"identity"`
	Status              NodeStatus   `json:"status"`
	InputArtifactIDs    []string     `json:"input_artifact_ids"`
	OutputArtifactIDs   []string     `json:"output_artifact_ids"`
	EvidenceArtifactIDs []string     `json:"evidence_artifact_ids"`
	StageID             *string      `json:"stage_id"`
}
type Edge struct {
	ID                  string   `json:"id"`
	RunID               string   `json:"run_id"`
	FromNodeID          string   `json:"from_node_id"`
	ToNodeID            string   `json:"to_node_id"`
	FromPortID          string   `json:"from_port_id"`
	ToPortID            string   `json:"to_port_id"`
	InputArtifactIDs    []string `json:"input_artifact_ids"`
	OutputArtifactIDs   []string `json:"output_artifact_ids"`
	EvidenceArtifactIDs []string `json:"evidence_artifact_ids"`
	Kind                string   `json:"kind"`
	Status              string   `json:"status"`
	Label               string   `json:"label"`
	Scope               string   `json:"scope"`
	CurrentValidity     string   `json:"current_validity"`
}
type Finding struct {
	ID              string          `json:"id"`
	RunID           string          `json:"run_id"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	Message         string          `json:"message"`
	NodeIDs         []string        `json:"node_ids"`
	EdgeIDs         []string        `json:"edge_ids"`
	ArtifactIDs     []string        `json:"artifact_ids"`
	RequirementRefs []string        `json:"requirement_refs"`
	OriginalNext    json.RawMessage `json:"original_next"`
}
type ObservationScope struct {
	Kind        string   `json:"kind"`
	HostVersion string   `json:"host_version"`
	HostPID     *int     `json:"host_pid"`
	StartedAt   *string  `json:"started_at"`
	FinishedAt  *string  `json:"finished_at"`
	RunIDs      []string `json:"run_ids"`
	Formula     string   `json:"formula"`
	Limitations []string `json:"limitations"`
}
type Metric struct {
	ObservationScope  *ObservationScope `json:"observation_scope,omitempty"`
	ID                string            `json:"id"`
	RunID             string            `json:"run_id"`
	Name              string            `json:"name"`
	Status            string            `json:"status"`
	Value             *float64          `json:"value"`
	Unit              string            `json:"unit"`
	Numerator         *float64          `json:"numerator"`
	Denominator       *float64          `json:"denominator"`
	SourceArtifactIDs []string          `json:"source_artifact_ids"`
	Gaps              []string          `json:"gaps"`
}

func graphIDs[T any](items []T, id func(T) string) (map[string]T, error) {
	result := map[string]T{}
	for _, item := range items {
		key := id(item)
		if !identity.MatchString(key) {
			return nil, fmt.Errorf("invalid graph id %q", key)
		}
		if _, ok := result[key]; ok {
			return nil, fmt.Errorf("duplicate graph id %q", key)
		}
		result[key] = item
	}
	return result, nil
}
func refsExist[T any](refs []string, items map[string]T) error {
	if refs == nil {
		return fmt.Errorf("graph reference array missing")
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			return fmt.Errorf("duplicate graph reference %q", ref)
		}
		seen[ref] = true
		if _, ok := items[ref]; !ok {
			return fmt.Errorf("graph reference %q missing", ref)
		}
	}
	return nil
}
func nonempty(value *string) bool { return value != nil && strings.TrimSpace(*value) != "" }
func oneOf(value string, values ...string) bool {
	for _, v := range values {
		if value == v {
			return true
		}
	}
	return false
}
func validateGraph(view View) error {
	g := view.Graph
	if view.SchemaVersion == 1 {
		if g != nil {
			return fmt.Errorf("schema 1 contains graph")
		}
		return nil
	}
	if g == nil || g.Objects == nil || g.Ports == nil || g.Repositories == nil || g.Nodes == nil || g.Artifacts == nil || g.Edges == nil || g.Findings == nil || g.Metrics == nil {
		return fmt.Errorf("schema 2 graph arrays missing")
	}
	objects, err := graphIDs(g.Objects, func(v Object) string { return v.ID })
	if err != nil {
		return err
	}
	ports, err := graphIDs(g.Ports, func(v Port) string { return v.ID })
	if err != nil {
		return err
	}
	repos, err := graphIDs(g.Repositories, func(v Repository) string { return v.ID })
	if err != nil {
		return err
	}
	nodes, err := graphIDs(g.Nodes, func(v Node) string { return v.ID })
	if err != nil {
		return err
	}
	artifacts, err := graphIDs(g.Artifacts, func(v Artifact) string { return v.ID })
	if err != nil {
		return err
	}
	edges, err := graphIDs(g.Edges, func(v Edge) string { return v.ID })
	if err != nil {
		return err
	}
	if _, err := graphIDs(g.Findings, func(v Finding) string { return v.ID }); err != nil {
		return err
	}
	if _, err := graphIDs(g.Metrics, func(v Metric) string { return v.ID }); err != nil {
		return err
	}
	runs := map[string]Run{}
	for _, run := range view.Runs {
		runs[run.ID] = run
	}
	namespaces := map[string]bool{}
	for _, o := range g.Objects {
		if o.Namespace == "" || namespaces[o.Namespace] || !oneOf(o.Scope, "shared", "repository") {
			return fmt.Errorf("object %s namespace duplicated or scope invalid", o.ID)
		}
		namespaces[o.Namespace] = true
		if err := refsExist(o.SourceArtifactIDs, artifacts); err != nil {
			return err
		}
	}
	for _, p := range g.Ports {
		if _, ok := objects[p.ObjectID]; !ok || !oneOf(p.Direction, "input", "output") {
			return fmt.Errorf("port %s owner or direction invalid", p.ID)
		}
		if err := refsExist(p.SourceArtifactIDs, artifacts); err != nil {
			return err
		}
	}
	for _, a := range g.Artifacts {
		if a.RepositoryID != nil {
			if _, ok := repos[*a.RepositoryID]; !ok {
				return fmt.Errorf("artifact %s repository missing", a.ID)
			}
		}
		if !oneOf(a.Status, "available", "unknown", "stale", "not_public") {
			return fmt.Errorf("artifact %s status invalid", a.ID)
		}
		if a.Status == "available" && (a.Source == nil || a.Source.SHA256 == nil || a.Source.Bytes == nil) {
			return fmt.Errorf("artifact %s available version binding missing", a.ID)
		}
		if a.Source != nil && a.Source.SHA256 != nil {
			hash, err := hex.DecodeString(*a.Source.SHA256)
			if err != nil || len(hash) != 32 || strings.ToLower(*a.Source.SHA256) != *a.Source.SHA256 {
				return fmt.Errorf("artifact %s digest invalid", a.ID)
			}
		}
		if a.Content != nil {
			if a.ContentEncoding != "utf-8" || a.Source == nil || a.Source.SHA256 == nil || a.Source.Bytes == nil {
				return fmt.Errorf("artifact %s content binding missing", a.ID)
			}
			hash := sha256.Sum256([]byte(*a.Content))
			if hex.EncodeToString(hash[:]) != *a.Source.SHA256 || int64(len(*a.Content)) != *a.Source.Bytes {
				return fmt.Errorf("artifact %s content version mismatch", a.ID)
			}
		}
		if a.PublicContent != nil {
			hash := sha256.Sum256([]byte(*a.PublicContent))
			if a.PublicSHA256 == nil || a.PublicBytes == nil || hex.EncodeToString(hash[:]) != *a.PublicSHA256 || int64(len(*a.PublicContent)) != *a.PublicBytes {
				return fmt.Errorf("artifact %s public version mismatch", a.ID)
			}
		} else if a.PublicSHA256 != nil || a.PublicBytes != nil {
			return fmt.Errorf("artifact %s public content missing", a.ID)
		}
		if a.Source != nil && a.Source.Bytes != nil && *a.Source.Bytes < 0 {
			return fmt.Errorf("artifact %s size invalid", a.ID)
		}
	}
	for _, n := range g.Nodes {
		if _, ok := runs[n.RunID]; !ok {
			return fmt.Errorf("node %s run missing", n.ID)
		}
		if _, ok := repos[n.RepositoryID]; !ok {
			return fmt.Errorf("node %s repository missing", n.ID)
		}
		if _, ok := objects[n.ObjectID]; !ok {
			return fmt.Errorf("node %s object missing", n.ID)
		}
		if !oneOf(n.Kind, "session", "pclass", "cli", "observation") || !oneOf(n.Identity.Status, "observed", "unknown") {
			return fmt.Errorf("node %s kind or identity invalid", n.ID)
		}
		if n.Identity.Status == "observed" && (n.Identity.Locator == nil || strings.TrimSpace(*n.Identity.Locator) == "" || (!nonempty(n.Identity.InvocationID) && !nonempty(n.Identity.AttemptID) && !nonempty(n.Identity.SessionID))) {
			return fmt.Errorf("node %s observed identity locator missing", n.ID)
		}
		if n.StageID != nil {
			found := false
			for _, s := range runs[n.RunID].Stages {
				if s.ID == *n.StageID {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("node %s stage missing", n.ID)
			}
		}
		for _, refs := range [][]string{n.InputArtifactIDs, n.OutputArtifactIDs, n.EvidenceArtifactIDs} {
			if err := refsExist(refs, artifacts); err != nil {
				return err
			}
		}
	}
	for _, r := range g.Repositories {
		if r.IndexStatus == "complete" && (r.Revision == nil || *r.Revision == "") {
			return fmt.Errorf("repository %s complete revision missing", r.ID)
		}
		if !oneOf(r.IndexStatus, "complete", "unknown", "not_public") || r.TrackedFiles == nil || r.SelectedFiles == nil || r.Gaps == nil {
			return fmt.Errorf("repository %s index invalid", r.ID)
		}
		files := append(append([]IndexFile{}, r.TrackedFiles...), r.SelectedFiles...)
		if _, err := graphIDs(files, func(f IndexFile) string { return f.ID }); err != nil {
			return err
		}
		for _, f := range files {
			if f.Path == "" || path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || f.Path == ".." || strings.HasPrefix(f.Path, "../") {
				return fmt.Errorf("index path %q invalid", f.Path)
			}
			if err := refsExist(f.ArtifactIDs, artifacts); err != nil {
				return err
			}
			if err := refsExist(f.NodeIDs, nodes); err != nil {
				return err
			}
		}
	}
	adjacency := map[string][]string{}
	indegree := map[string]int{}
	for id := range nodes {
		indegree[id] = 0
	}
	for _, e := range g.Edges {
		from, fromOK := nodes[e.FromNodeID]
		to, toOK := nodes[e.ToNodeID]
		fp, fpOK := ports[e.FromPortID]
		tp, tpOK := ports[e.ToPortID]
		if !fromOK || !toOK || !fpOK || !tpOK || fp.ObjectID != from.ObjectID || tp.ObjectID != to.ObjectID || fp.Direction != "output" || tp.Direction != "input" {
			return fmt.Errorf("edge %s endpoint or port invalid", e.ID)
		}
		if _, ok := runs[e.RunID]; !ok {
			return fmt.Errorf("edge %s run missing", e.ID)
		}
		if !oneOf(e.Kind, "handoff", "diagnostic") || !oneOf(e.Status, "observed", "unknown", "failed", "not_applicable") || !oneOf(e.CurrentValidity, "current", "stale", "unknown") {
			return fmt.Errorf("edge %s status invalid", e.ID)
		}
		scope := "within_repository"
		if from.RepositoryID != to.RepositoryID {
			scope = "cross_repository"
		}
		if e.Scope != scope {
			return fmt.Errorf("edge %s scope mismatches endpoints", e.ID)
		}
		for _, refs := range [][]string{e.InputArtifactIDs, e.OutputArtifactIDs, e.EvidenceArtifactIDs} {
			if err := refsExist(refs, artifacts); err != nil {
				return err
			}
		}
		if e.Status == "observed" {
			if e.Kind != "handoff" {
				return fmt.Errorf("diagnostic %s claims handoff", e.ID)
			}
			if e.Scope == "cross_repository" && objects[from.ObjectID].Namespace != "soodles.loop" && objects[to.ObjectID].Namespace != "soodles.loop" {
				return fmt.Errorf("cross repository edge %s lacks LoopManager occurrence", e.ID)
			}
			producerRefs := map[string]bool{}
			for _, id := range append(append([]string{}, from.InputArtifactIDs...), from.OutputArtifactIDs...) {
				producerRefs[id] = true
			}
			for _, id := range e.InputArtifactIDs {
				if !producerRefs[id] {
					return fmt.Errorf("edge %s producer artifact mismatch", e.ID)
				}
			}
			consumerRefs := map[string]bool{}
			for _, id := range append(append([]string{}, to.InputArtifactIDs...), to.OutputArtifactIDs...) {
				consumerRefs[id] = true
			}
			for _, id := range e.OutputArtifactIDs {
				if !consumerRefs[id] {
					return fmt.Errorf("edge %s consumer artifact mismatch", e.ID)
				}
			}
			for _, id := range e.EvidenceArtifactIDs {
				if artifacts[id].Status == "unknown" {
					return fmt.Errorf("edge %s evidence unavailable", e.ID)
				}
			}
			if from.Identity.Status != "observed" || to.Identity.Status != "observed" || len(e.InputArtifactIDs) == 0 || len(e.OutputArtifactIDs) == 0 || len(e.EvidenceArtifactIDs) == 0 {
				return fmt.Errorf("edge %s observed handoff evidence missing", e.ID)
			}
		}
		if e.Kind == "handoff" {
			adjacency[e.FromNodeID] = append(adjacency[e.FromNodeID], e.ToNodeID)
			indegree[e.ToNodeID]++
		}
	}
	queue := []string{}
	for id, n := range indegree {
		if n == 0 {
			queue = append(queue, id)
		}
	}
	count := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		count++
		for _, next := range adjacency[id] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if count != len(nodes) {
		return fmt.Errorf("causal graph contains cycle")
	}
	for _, f := range g.Findings {
		if _, ok := runs[f.RunID]; !ok {
			return fmt.Errorf("finding %s run missing", f.ID)
		}
		if !oneOf(f.Kind, "mechanical", "semantic", "acceptance", "evidence_gap") || !oneOf(f.Status, "failed", "unknown", "not_applicable", "resolved") {
			return fmt.Errorf("finding %s kind or status invalid", f.ID)
		}
		if err := refsExist(f.NodeIDs, nodes); err != nil {
			return err
		}
		if err := refsExist(f.EdgeIDs, edges); err != nil {
			return err
		}
		if err := refsExist(f.ArtifactIDs, artifacts); err != nil {
			return err
		}
	}
	for _, m := range g.Metrics {
		if m.ObservationScope != nil {
			s := m.ObservationScope
			if s.Kind != "historical_measurement" || (s.HostPID != nil && *s.HostPID <= 0) {
				return fmt.Errorf("metric %s observation scope invalid", m.ID)
			}
		}
		if oneOf(m.Unit, "ratio", "percent") && m.Status == "observed" {
			if m.Numerator == nil || m.Denominator == nil || *m.Denominator <= 0 || m.Value == nil {
				return fmt.Errorf("metric %s ratio denominator incomplete", m.ID)
			}
			expected := *m.Numerator / *m.Denominator
			if m.Unit == "percent" {
				expected *= 100
			}
			if math.Abs(*m.Value-expected) > 1e-9*math.Max(math.Abs(*m.Value), math.Abs(expected)) {
				return fmt.Errorf("metric %s ratio formula mismatch", m.ID)
			}
		}
		for _, v := range []*float64{m.Numerator, m.Denominator} {
			if v != nil && *v < 0 {
				return fmt.Errorf("metric %s negative value", m.ID)
			}
		}
		if m.Numerator != nil && m.Denominator != nil && *m.Numerator > *m.Denominator {
			return fmt.Errorf("metric %s numerator exceeds denominator", m.ID)
		}
		if _, ok := runs[m.RunID]; !ok {
			return fmt.Errorf("metric %s run missing", m.ID)
		}
		if !oneOf(m.Status, "observed", "unknown", "not_applicable") {
			return fmt.Errorf("metric %s status invalid", m.ID)
		}
		if m.Status != "observed" && m.Value != nil {
			return fmt.Errorf("metric %s unknown value asserted", m.ID)
		}
		if m.Denominator != nil && *m.Denominator == 0 && m.Status != "not_applicable" {
			return fmt.Errorf("metric %s zero denominator", m.ID)
		}
		if err := refsExist(m.SourceArtifactIDs, artifacts); err != nil {
			return err
		}
	}
	return nil
}
