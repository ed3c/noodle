package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// This is a projection check, not a second handoff verifier. The original
// owner verifies the accepted chain; these references never select a launcher
// or replace the registered root request used by actions.
func validateContextActiveStage(o contextObservation) error {
	stage := o.ActiveStage
	if stage == nil || stage.Handoffs == nil || stage.Composition.ID != o.Composition.ID || stage.Composition.Definition != o.Composition.Definition || !contextStageName(stage.Composition.StageID) {
		return fmt.Errorf("active stage composition differs from registered entry")
	}
	if !contextProjectionRef(stage.Request) {
		return fmt.Errorf("active stage request reference invalid")
	}
	seen := map[string]bool{}
	for _, ref := range stage.Handoffs {
		if !contextProjectionRef(ref) || seen[ref.Path] {
			return fmt.Errorf("active stage handoff reference invalid or repeated")
		}
		seen[ref.Path] = true
	}
	if len(stage.Handoffs) == 0 {
		if stage.Request != o.Request || stage.Composition != *o.Composition {
			return fmt.Errorf("active stage changed without handoff references")
		}
	} else if stage.Request.Path == o.Request.Path || stage.Composition.StageID == o.Composition.StageID {
		return fmt.Errorf("active stage handoff retained entry identity")
	}
	return nil
}

func contextProjectionRef(ref contextFileRef) bool {
	return filepath.IsAbs(ref.Path) && !strings.ContainsRune(ref.Path, 0) && contextHashPattern.MatchString(ref.SHA256)
}

func contextStageName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
