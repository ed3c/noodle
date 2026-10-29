package runtime

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// TaskDeliveryAcceptance is emitted by the original outer task flow after all
// admitted delivery conditions have been checked against the integrated head.
type TaskDeliveryAcceptance struct {
	Schema        int                     `json:"schema"`
	OrderID       string                  `json:"order_id"`
	SessionID     string                  `json:"session_id"`
	CandidateHead string                  `json:"candidate_head"`
	Status        string                  `json:"status"`
	Conditions    []TaskDeliveryCondition `json:"conditions"`
}

type TaskDeliveryCondition struct {
	Condition string                   `json:"condition"`
	Status    string                   `json:"status"`
	Evidence  []SessionReadbackFileRef `json:"evidence"`
}

func ReadTaskDeliveryAcceptance(ref SessionReadbackFileRef) (TaskDeliveryAcceptance, error) {
	var value TaskDeliveryAcceptance
	actual, err := ReadSessionReadbackFile(ref.Path, &value)
	if err != nil {
		return value, err
	}
	if actual != ref || !filepath.IsAbs(ref.Path) {
		return value, fmt.Errorf("outer acceptance source binding differs")
	}
	if value.Schema != 1 || value.OrderID == "" || value.SessionID == "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(value.CandidateHead) || value.Status != "PASS" || len(value.Conditions) == 0 {
		return value, fmt.Errorf("outer task delivery acceptance is incomplete")
	}
	seen := map[string]bool{}
	for _, condition := range value.Conditions {
		if strings.TrimSpace(condition.Condition) == "" || seen[condition.Condition] || condition.Status != "PASS" || len(condition.Evidence) == 0 {
			return value, fmt.Errorf("outer acceptance has missing, repeated or unresolved conditions")
		}
		seen[condition.Condition] = true
		for _, evidence := range condition.Evidence {
			if !filepath.IsAbs(evidence.Path) {
				return value, fmt.Errorf("outer acceptance evidence path is not absolute")
			}
			if err := VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"condition_evidence": evidence}); err != nil {
				return value, err
			}
		}
	}
	return value, VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"outer_acceptance": ref})
}
