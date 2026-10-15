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
	imp := &cobra.Command{Use: "import --file FILE", Short: "Validate and merge schema_version 1 runs by ID; preserve repository and mode", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "changed": changed, "run_count": count, "registry_path": loopview.Path(dir)})
	}}
	imp.Flags().StringVar(&file, "file", "", "JSON display projection (schema_version:1, runs:[]); refs and next are display only")
	cmd.AddCommand(imp)
	cmd.AddCommand(newHostViewBindCmd())
	cmd.AddCommand(&cobra.Command{Use: "inspect", Short: "Read stored loop display projections without contacting Host", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		view, err := loopview.Load(dir)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(view)
	}})
	cmd.AddCommand(&cobra.Command{Use: "capabilities", Short: "Read the display projection capability", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "offline_import": true, "readonly_web": true, "path": "/loops", "api_path": "/api/loops"})
	}})
	return cmd
}
