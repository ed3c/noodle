package loop

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/poteto/noodle/dispatcher"
)

// The original local-task owner selects the frozen inputs. This boundary does
// not infer files from prompt text or turn a host path into a guest path.
func (l *Loop) remoteTaskInputs(cand dispatchCandidate) (*dispatcher.InstructionBundleRef, string, error) {
	if cand.Stage.Runtime != "openshell" {
		return nil, "", nil
	}
	var prompt struct {
		Kind    string `json:"kind"`
		Effort  string `json:"model_reasoning_effort"`
		Context struct {
			Materialization *dispatcher.InstructionBundleRef `json:"materialization"`
		} `json:"instruction_context"`
	}
	err := json.Unmarshal([]byte(cand.Stage.Prompt), &prompt)
	if cand.Stage.TaskKey != "local-task" && prompt.Kind != "local_task" && prompt.Context.Materialization == nil {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("OpenShell local task inputs invalid: %w", err)
	}
	order, ok := l.canonical.Orders[cand.OrderID]
	if !ok || cand.StageIndex != 0 || len(order.Stages) != 1 {
		return nil, "", fmt.Errorf("OpenShell input owner missing")
	}
	stage := order.Stages[0]
	if !l.isAdmittedLocalTask(order, stage) || stage.Prompt != cand.Stage.Prompt || stage.Provider != cand.Stage.Provider || stage.Model != cand.Stage.Model || stage.Runtime != cand.Stage.Runtime || stage.TaskKey != cand.Stage.TaskKey || stage.Skill != cand.Stage.Skill {
		return nil, "", fmt.Errorf("OpenShell inputs differ from original local task")
	}
	ref := prompt.Context.Materialization
	if ref == nil || !filepath.IsAbs(ref.Path) || filepath.Clean(ref.Path) != ref.Path || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(ref.SHA256) {
		return nil, "", fmt.Errorf("OpenShell frozen instruction reference missing or invalid")
	}
	if strings.TrimSpace(prompt.Effort) != prompt.Effort {
		return nil, "", fmt.Errorf("OpenShell reasoning effort contains surrounding whitespace")
	}
	return ref, prompt.Effort, nil
}
