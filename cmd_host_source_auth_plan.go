package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/statever"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

func newHostSourceAuthPlanCmd() *cobra.Command {
	var requestFile, sessionFile string
	cmd := &cobra.Command{Use: "source-auth-plan", Short: "Prepare a read-only Source client-auth proposal for the original task", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := prepareHostSourceAuthPlan(requestFile, sessionFile)
		if outErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); outErr != nil {
			return outErr
		}
		return err
	}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original native attach request")
	cmd.Flags().StringVar(&sessionFile, "session-file", "", "Original spawn.json when task history has multiple sessions")
	_ = cmd.MarkFlagRequired("request-file")
	return cmd
}

func prepareHostSourceAuthPlan(requestFile, sessionFile string) (result loopruntime.SourceAuthPlan, err error) {
	result = loopruntime.SourceAuthPlan{Schema: 1, Status: "refused", Effects: []string{}, Sources: map[string]loopruntime.SessionReadbackFileRef{}}
	owner, err := readHostOwnerBinding(requestFile)
	if err != nil {
		return result, err
	}
	sources := owner.Sources
	r := owner.Current
	configPath := r.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(r.ProjectDir, ".noodle.toml")
	}
	sources["config"], err = loopruntime.ReadSessionReadbackFile(configPath, nil)
	if err != nil {
		return result, err
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return result, err
	}
	if hostDigest(raw) != r.ConfigSHA256 || sources["config"].SHA256 != r.ConfigSHA256 {
		return result, fmt.Errorf("original task config changed")
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return result, err
	}
	if !filepath.IsAbs(cfg.Runtime.OpenShell.Profile) {
		return result, fmt.Errorf("original absolute task profile is unavailable")
	}
	var snapshot reducer.DurableSnapshot
	sources["canonical_snapshot"], err = loopruntime.ReadSessionReadbackFile(filepath.Join(r.ProjectDir, ".noodle", "state.snapshot.json"), &snapshot)
	if err != nil {
		return result, err
	}
	if snapshot.State.SchemaVersion != statever.Current {
		return result, fmt.Errorf("original canonical snapshot schema differs")
	}
	selected := ""
	if sessionFile != "" {
		var spawn struct {
			SessionID string `json:"session_id"`
		}
		sources["selected_session"], err = loopruntime.ReadSessionReadbackFile(sessionFile, &spawn)
		if err != nil {
			return result, err
		}
		selected = spawn.SessionID
		if selected == "" {
			return result, fmt.Errorf("selected original spawn has no session identity")
		}
	}
	var orderID, sessionID, worktree, provider, model, canonicalAttemptID, originalStage string
	count := 0
	for key, order := range snapshot.State.Orders {
		for stageIndex, stage := range order.Stages {
			for _, attempt := range stage.Attempts {
				if stage.Runtime != "openshell" || (selected != "" && attempt.SessionID != selected) {
					continue
				}
				if key != order.OrderID || stageIndex != stage.StageIndex || attempt.SessionID == "" || filepath.Base(attempt.SessionID) != attempt.SessionID || attempt.SessionID == "." || attempt.SessionID == ".." || attempt.WorktreeName == "" || filepath.Base(attempt.WorktreeName) != attempt.WorktreeName || attempt.WorktreeName == "." || attempt.WorktreeName == ".." {
					return result, fmt.Errorf("original canonical task identity differs")
				}
				count++
				canonicalAttemptID, originalStage = attempt.AttemptID, strconv.Itoa(stageIndex)
				if canonicalAttemptID == "" {
					return result, fmt.Errorf("original canonical attempt identity missing")
				}
				orderID, sessionID, provider, model = order.OrderID, attempt.SessionID, stage.Provider, stage.Model
				worktree = filepath.Join(r.ProjectDir, ".worktrees", attempt.WorktreeName)
			}
		}
	}
	if count != 1 {
		result.Status = "unknown"
		result.Sources = sources
		result.Requires = []string{"unique_original_session_file"}
		if err = loopruntime.VerifySessionReadbackFiles(sources); err != nil {
			result.Status = "refused"
			return result, err
		}
		return result, nil
	}
	dir := filepath.Join(r.ProjectDir, ".noodle", "sessions", sessionID)
	var spawn struct {
		OrderID      string `json:"order_id"`
		StageIndex   string `json:"stage_index"`
		SessionID    string `json:"session_id"`
		WorktreePath string `json:"worktree_path"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Runtime      string `json:"runtime"`
	}
	sources["spawn"], err = loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), &spawn)
	if err != nil {
		return result, err
	}
	if (sessionFile != "" && sources["selected_session"] != sources["spawn"]) || spawn.SessionID != sessionID || spawn.WorktreePath != worktree || spawn.Provider != provider || spawn.Model != model || spawn.Runtime != "openshell" || spawn.OrderID != orderID || spawn.StageIndex != originalStage {
		return result, fmt.Errorf("original task spawn differs from canonical attempt")
	}
	templatePath, err := goalHostTemplatePath()
	if err != nil {
		return result, err
	}
	var template goalHostTemplateRegistration
	sources["template_registration"], err = loopruntime.ReadSessionReadbackFile(templatePath, &template)
	if err != nil {
		return result, err
	}
	sources["registered_template"], err = loopruntime.ReadSessionReadbackFile(template.Profile.Path, nil)
	if err != nil {
		return result, err
	}
	if template.Schema != 1 || sources["registered_template"] != template.Profile {
		return result, fmt.Errorf("registered Source auth template changed")
	}
	result, err = loopruntime.InspectSourceAuthPlan(cfg.Runtime.OpenShell.Profile, template.Profile.Path, filepath.Join(dir, "openshell-dispatch.json"), r.ContextID, owner.HostRoot)
	if err != nil {
		return result, err
	}
	if result.TemplatePolicySHA256 != template.PolicySHA256 || result.Sources["template_profile"] != template.Profile {
		result.Status = "refused"
		return result, fmt.Errorf("registered Source auth template dependencies changed")
	}
	result.CanonicalAttemptID = canonicalAttemptID
	if result.OrderID != orderID || result.SessionID != sessionID || result.WorktreePath != worktree {
		result.Status = "refused"
		return result, fmt.Errorf("Source auth proposal does not match original canonical task")
	}
	for name, ref := range sources {
		result.Sources[name] = ref
	}
	if err = loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		result.Status = "refused"
		return result, err
	}
	return result, nil
}
