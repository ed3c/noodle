package main

import (
	"encoding/json"
	"fmt"

	"github.com/poteto/noodle/internal/loopview"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

func newHostViewsCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "views", Short: "Import and inspect readonly loop display projections without starting Host"}
	var file string
	imp := &cobra.Command{Use: "import --file FILE", Short: "Validate and merge versioned display runs and DAG evidence", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if file == "" {
			return fmt.Errorf("loop view input file missing")
		}
		view, err := loopview.Read(file)
		if err != nil {
			return err
		}
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		changed, count, err := loopview.Import(dir, view)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": view.SchemaVersion, "changed": changed, "run_count": count, "registry_path": loopview.Path(dir)})
	}}
	imp.Flags().StringVar(&file, "file", "", "JSON display projection (schema_version 1 or 2); refs and next are display only")
	cmd.AddCommand(imp)
	cmd.AddCommand(newHostViewBindCmd(app))
	cmd.AddCommand(newHostReviewSelectionCmd(app))
	var includeWorkspace bool
	inspect := &cobra.Command{Use: "inspect", Short: "Read stored display and native workspace observations without contacting Host", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		inventory, err := inspectHostWorkspace(hostRequest{ProjectDir: app.projectDir})
		if err != nil {
			return err
		}
		dir, err := hostReadDirectory()
		if err != nil {
			return err
		}
		view, err := loopview.Load(dir)
		if err != nil {
			inventory.Gaps = append(inventory.Gaps, loop.InspectionGap{Path: loopview.Path(dir), Code: "unreadable", Detail: err.Error()})
		} else {
			if err = enrichHostView(dir, &view, hostViewObservation{Receipts: inventory.Contexts}); err != nil {
				inventory.Gaps = append(inventory.Gaps, loop.InspectionGap{Path: viewBindingsPath(dir), Code: "invalid", Detail: err.Error()})
			}
		}
		if !includeWorkspace {
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(view)
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			loopview.View
			WorkspaceObservation hostWorkspaceInspection `json:"workspace_observation"`
		}{view, inventory})
	}}
	inspect.Flags().BoolVar(&includeWorkspace, "include-workspace", false, "Include read-only native workspace candidates and gaps")
	cmd.AddCommand(inspect)
	cmd.AddCommand(&cobra.Command{Use: "capabilities", Short: "Read the display projection capability", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 2, "supported_schema_versions": []int{1, 2}, "graph_api_path": "/api/loops/graph", "offline_import": true, "readonly_web": true, "path": "/loops", "api_path": "/api/loops"})
	}})
	return cmd
}

func newHostReviewSelectionCmd(app *App) *cobra.Command {
	var request, id string
	cmd := &cobra.Command{Use: "review-selections", Short: "Read explicit UI review references without dispatch", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := hostReadDirectory()
		if err != nil {
			return err
		}
		result, err := loopview.InspectWorkspaceReviewSelections(dir, app.projectDir, request, id)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&request, "request-file", "", "Original loop request path")
	cmd.Flags().StringVar(&id, "id", "", "Exact review reference; omission lists references for this request")
	return cmd
}
