package main

import (
	"encoding/json"
	"fmt"

	"github.com/poteto/noodle/internal/loopview"
	"github.com/spf13/cobra"
)

func newHostViewsCmd() *cobra.Command {
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
	cmd.AddCommand(&cobra.Command{Use: "migrate", Short: "Preserve the existing projection and move bodies into immutable details", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		changed, err := loopview.Migrate(dir)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"changed": changed, "registry_path": loopview.Path(dir)})
	}})
	cmd.AddCommand(newHostViewBindCmd())
	cmd.AddCommand(newHostReviewSelectionCmd())
	var runID, repoID string
	inspect := &cobra.Command{Use: "inspect", Short: "Read the projection summary or one immutable detail without contacting Host", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if runID != "" && repoID != "" {
			return fmt.Errorf("run and repository selectors conflict")
		}
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		view, err := loopview.LoadSummary(dir)
		if err != nil {
			return err
		}
		if runID != "" {
			for _, run := range view.Runs {
				if run.ID == runID {
					body, err := loopview.RunDetail(dir, run)
					if err != nil {
						return err
					}
					return json.NewEncoder(cmd.OutOrStdout()).Encode(body)
				}
			}
			return fmt.Errorf("registered run not found")
		}
		if repoID != "" {
			if view.Graph != nil {
				for _, repo := range view.Graph.Repositories {
					if repo.ID == repoID {
						body, err := loopview.RepositoryDetail(dir, repo)
						if err != nil {
							return err
						}
						return json.NewEncoder(cmd.OutOrStdout()).Encode(body)
					}
				}
			}
			return fmt.Errorf("registered repository not found")
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(view)
	}}
	inspect.Flags().StringVar(&runID, "run-id", "", "Exact run whose complete imported body is read")
	inspect.Flags().StringVar(&repoID, "repository-id", "", "Exact repository snapshot whose complete index is read")
	cmd.AddCommand(inspect)

	cmd.AddCommand(&cobra.Command{Use: "capabilities", Short: "Read the display projection capability", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 2, "supported_schema_versions": []int{1, 2}, "graph_api_path": "/api/loops/graph", "offline_import": true, "immutable_details": true, "registry_version": 1, "readonly_web": true, "path": "/loops", "api_path": "/api/loops"})
	}})
	return cmd
}

func newHostReviewSelectionCmd() *cobra.Command {
	var request, id string
	cmd := &cobra.Command{Use: "review-selections", Short: "Read explicit UI review references without dispatch", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if request == "" {
			return fmt.Errorf("original request file required")
		}
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		result, err := loopview.InspectReviewSelections(dir, request, id)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&request, "request-file", "", "Original loop request path")
	cmd.Flags().StringVar(&id, "id", "", "Exact review reference; omission lists references for this request")
	return cmd
}
