package runtime

import (
	"fmt"
	"strings"

	"github.com/poteto/noodle/internal/admission"
)

// GoalExecutionConstraints is restrictive semantic input from the original
// Goal, never an effect grant or a per-task resource estimate.
type GoalExecutionConstraints struct {
	DependencyInstallation string `json:"dependency_installation"`
}

func OpenShellResourceBudgetClass(p OpenShellProfile) (string, error) {
	var cfg struct {
		Budget struct{ Class string } `toml:"noodle_resource_budget"`
	}
	if err := readOpenShellResourcePolicy(p, &cfg); err != nil {
		return "", err
	}
	return cfg.Budget.Class, nil
}

func ReadOpenShellGoalExecutionConstraints(p OpenShellProfile, goal SessionReadbackFileRef) (*GoalExecutionConstraints, error) {
	class, err := OpenShellResourceBudgetClass(p)
	if err != nil {
		return nil, err
	}
	if class != QualificationGoalResourceBudget {
		return nil, nil
	}
	fail := func(reason string) error {
		return fmt.Errorf("%w: small qualification Goal restrictions: %s", admission.ErrDeferred, reason)
	}
	var document struct {
		Goal        string                    `json:"goal"`
		Constraints *GoalExecutionConstraints `json:"execution_constraints"`
	}
	actual, err := ReadSessionReadbackFile(goal.Path, &document)
	if err != nil {
		return nil, fail("original contract unavailable: " + err.Error())
	}
	if actual != goal {
		return nil, fail("original Source changed")
	}
	if strings.TrimSpace(document.Goal) == "" || document.Constraints == nil || document.Constraints.DependencyInstallation != "forbidden" {
		return nil, fail("original Goal must explicitly forbid dependency installation")
	}
	return document.Constraints, nil
}
