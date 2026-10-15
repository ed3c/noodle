package loopview

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

func mergeItems[T any](old, incoming []T, id func(T) string, invariant func(T, T) bool) ([]T, error) {
	byID := map[string]T{}
	for _, v := range old {
		byID[id(v)] = v
	}
	for _, v := range incoming {
		key := id(v)
		if prior, ok := byID[key]; ok && invariant != nil && !invariant(prior, v) {
			return nil, fmt.Errorf("graph identity %s changed", key)
		}
		byID[key] = v
	}
	result := make([]T, 0, len(byID))
	for _, v := range byID {
		result = append(result, v)
	}
	sort.Slice(result, func(i, j int) bool { return id(result[i]) < id(result[j]) })
	return result, nil
}
func keepOtherRuns[T any](values []T, runs map[string]bool, runID func(T) string) []T {
	result := []T{}
	for _, v := range values {
		if !runs[runID(v)] {
			result = append(result, v)
		}
	}
	return result
}
func mergeGraph(current *View, incoming View) error {
	if incoming.Graph == nil {
		return nil
	}
	if current.Graph == nil {
		current.Graph = &Graph{}
	}
	current.SchemaVersion = 2
	old, g := current.Graph, incoming.Graph
	var err error
	old.Objects, err = mergeItems(old.Objects, g.Objects, func(v Object) string { return v.ID }, func(a, b Object) bool { return a.Namespace == b.Namespace && a.Kind == b.Kind && a.Scope == b.Scope })
	if err != nil {
		return err
	}
	old.Ports, err = mergeItems(old.Ports, g.Ports, func(v Port) string { return v.ID }, func(a, b Port) bool {
		return a.ObjectID == b.ObjectID && a.Direction == b.Direction && a.Schema == b.Schema && a.EffectScope == b.EffectScope
	})
	if err != nil {
		return err
	}
	previousRepositories := old.Repositories
	old.Repositories, err = mergeItems(old.Repositories, g.Repositories, func(v Repository) string { return v.ID }, func(a, b Repository) bool { return a.Root == b.Root && reflect.DeepEqual(a.Revision, b.Revision) })
	if err != nil {
		return err
	}
	old.Artifacts, err = mergeItems(old.Artifacts, g.Artifacts, func(v Artifact) string { return v.ID }, func(a, b Artifact) bool {
		if !reflect.DeepEqual(a.RepositoryID, b.RepositoryID) {
			return false
		}
		if a.Source == nil || b.Source == nil {
			return a.Source == nil && b.Source == nil
		}
		return a.Source.Path == b.Source.Path && reflect.DeepEqual(a.Source.SHA256, b.Source.SHA256) &&
			(a.Source.Bytes == nil || b.Source.Bytes == nil || *a.Source.Bytes == *b.Source.Bytes)
	})
	if err != nil {
		return err
	}
	refreshed := map[string]bool{}
	for _, r := range incoming.Runs {
		refreshed[r.ID] = true
	}
	old.Nodes, _ = mergeItems(keepOtherRuns(old.Nodes, refreshed, func(v Node) string { return v.RunID }), g.Nodes, func(v Node) string { return v.ID }, nil)
	old.Edges, _ = mergeItems(keepOtherRuns(old.Edges, refreshed, func(v Edge) string { return v.RunID }), g.Edges, func(v Edge) string { return v.ID }, nil)
	old.Findings, _ = mergeItems(keepOtherRuns(old.Findings, refreshed, func(v Finding) string { return v.RunID }), g.Findings, func(v Finding) string { return v.ID }, nil)
	old.Metrics, _ = mergeItems(keepOtherRuns(old.Metrics, refreshed, func(v Metric) string { return v.RunID }), g.Metrics, func(v Metric) string { return v.ID }, nil)
	if err := retainIndexAssociations(old, previousRepositories); err != nil {
		return err
	}
	return nil
}

// Metadata removes bodies without changing original version identities.
func Metadata(view View) View {
	if view.Graph == nil {
		return view
	}
	graph := *view.Graph
	graph.Artifacts = append([]Artifact{}, graph.Artifacts...)
	for i := range graph.Artifacts {
		graph.Artifacts[i].Content = nil
		graph.Artifacts[i].PublicContent = nil
	}

	view.Runs = append([]Run{}, view.Runs...)
	for i := range view.Runs {
		run := &view.Runs[i]
		run.Stages = append([]Stage{}, run.Stages...)
		run.Cards = append([]Card{}, run.Cards...)
		run.Notes = append([]Note{}, run.Notes...)
		for j := range run.Stages {
			run.Stages[j].Prompt = ""
		}
		for j := range run.Cards {
			run.Cards[j].Body = ""
		}
		for j := range run.Notes {
			run.Notes[j].Body = ""
		}
	}
	view.Graph = &graph
	return view
}

func validateImport(view View) error {
	data, err := json.Marshal(view)
	if err != nil {
		return err
	}
	_, err = Decode(data)
	return err
}

func unionRefs(a, b []string) []string {
	set := map[string]bool{}
	for _, id := range append(append([]string{}, a...), b...) {
		set[id] = true
	}
	result := make([]string, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
func retainIndexAssociations(graph *Graph, prior []Repository) error {
	old := map[string]Repository{}
	for _, r := range prior {
		old[r.ID] = r
	}
	nodes := map[string]bool{}
	for _, n := range graph.Nodes {
		nodes[n.ID] = true
	}
	for i := range graph.Repositories {
		repo := &graph.Repositories[i]
		previous, ok := old[repo.ID]
		if !ok {
			continue
		}
		for _, pair := range []struct {
			current  *[]IndexFile
			previous []IndexFile
		}{{&repo.TrackedFiles, previous.TrackedFiles}, {&repo.SelectedFiles, previous.SelectedFiles}} {
			existing := map[string]IndexFile{}
			for _, f := range pair.previous {
				existing[f.ID] = f
			}

			incomingIDs := map[string]bool{}
			for _, file := range *pair.current {
				incomingIDs[file.ID] = true
			}
			for _, file := range pair.previous {
				if !incomingIDs[file.ID] {
					*pair.current = append(*pair.current, file)
				}
			}
			for j := range *pair.current {
				f := &(*pair.current)[j]
				if p, found := existing[f.ID]; found {
					if p.Path != f.Path || !reflect.DeepEqual(p.GitBlob, f.GitBlob) || !reflect.DeepEqual(p.GitMode, f.GitMode) {
						return fmt.Errorf("index identity %s changed", f.ID)
					}
					f.ArtifactIDs = unionRefs(p.ArtifactIDs, f.ArtifactIDs)
					f.NodeIDs = unionRefs(p.NodeIDs, f.NodeIDs)
				}
				valid := []string{}
				for _, id := range f.NodeIDs {
					if nodes[id] {
						valid = append(valid, id)
					}
				}
				f.NodeIDs = valid
			}
		}
	}
	return nil
}
