package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/cmdmeta"
	"github.com/poteto/noodle/skill"
	"github.com/spf13/cobra"
)

func newSkillsCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: cmdmeta.Short("skills"),
	}
	cmd.AddCommand(newSkillsListCmd(app))
	return cmd
}

func newSkillsListCmd(app *App) *cobra.Command {
	var outputJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: cmdmeta.Short("skills", "list"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if outputJSON {
				return runSkillsListJSON(app)
			}
			return runSkillsList(app)
		},
	}
	cmd.Flags().BoolVar(&outputJSON, "json", false, "emit a scoped, read-only resolver snapshot with Skill content digests")
	return cmd
}

func runSkillsList(app *App) error {
	resolver := skill.Resolver{SearchPaths: app.Config.Skills.Paths}
	infos, err := resolver.List()
	if err != nil {
		return err
	}

	for _, info := range infos {
		fmt.Fprintf(
			os.Stdout,
			"%s\t%s\t%t\t%s\n",
			info.Name,
			info.SourcePath,
			info.HasSkillMD,
			info.Path,
		)
	}

	return nil
}

type resolvedSkillReceipt struct {
	Name          string   `json:"name"`
	SourcePath    string   `json:"source_path"`
	Path          string   `json:"path"`
	SkillMDSHA256 string   `json:"skill_md_sha256"`
	TreeSHA256    string   `json:"tree_sha256"`
	ShadowedPaths []string `json:"shadowed_paths"`
}

type skillResolutionReceipt struct {
	Protocol                        string                 `json:"protocol"`
	Status                          string                 `json:"status"`
	SearchPaths                     []string               `json:"search_paths"`
	Skills                          []resolvedSkillReceipt `json:"skills"`
	CollisionDetected               bool                   `json:"collision_detected"`
	ActualWorkerSessionObserved     bool                   `json:"actual_worker_session_observed"`
	EffectiveAgentCatalogVerified  bool                   `json:"effective_agent_catalog_verified"`
	GlobalSkillInheritanceExcluded bool                   `json:"global_skill_inheritance_excluded"`
	EffectAuthority                 bool                   `json:"effect_authority"`
}

func runSkillsListJSON(app *App) error {
	resolver := skill.Resolver{SearchPaths: app.Config.Skills.Paths}
	infos, err := resolver.List()
	if err != nil {
		return err
	}
	out := skillResolutionReceipt{
		Protocol: "noodle/skill-resolution-v1",
		Status: "CONFIGURED_RESOLVER_SNAPSHOT",
		SearchPaths: append([]string{}, app.Config.Skills.Paths...),
		Skills: make([]resolvedSkillReceipt, 0, len(infos)),
	}
	for _, info := range infos {
		raw, err := os.ReadFile(filepath.Join(info.Path, "SKILL.md"))
		if err != nil {
			return fmt.Errorf("read %s: %w", info.Path, err)
		}
		entry := sha256.Sum256(raw)
		tree, err := skill.TreeSHA256(info.Path)
		if err != nil {
			return fmt.Errorf("digest selected Skill %s: %w", info.Name, err)
		}
		shadowed := make([]string, 0)
		seen := map[string]bool{info.Path: true}
		// Reuse the *same Resolver* one configured path at a time. Never
		// reconstruct path expansion or invent alternative precedence rules.
		for _, searchPath := range app.Config.Skills.Paths {
			selected, err := (skill.Resolver{SearchPaths: []string{searchPath}}).Resolve(info.Name)
			if errors.Is(err, skill.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if !seen[selected.Path] {
				shadowed = append(shadowed, selected.Path)
				seen[selected.Path] = true
			}
		}
		if len(shadowed) > 0 {
			out.CollisionDetected = true
			out.Status = "SHADOWED_SKILL_SOURCES"
		}
		out.Skills = append(out.Skills, resolvedSkillReceipt{
			Name: info.Name, SourcePath: info.SourcePath, Path: info.Path,
			SkillMDSHA256: hex.EncodeToString(entry[:]), TreeSHA256: tree,
			ShadowedPaths: shadowed,
		})
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, string(data)); err != nil {
		return err
	}
	// Preserve an exact machine-readable negative result, but refuse to
	// count a first-match-wins collision as an unambiguous Skill resolution.
	if out.CollisionDetected {
		return fmt.Errorf("duplicate Skill providers; resolution is shadowed")
	}
	return nil
}
