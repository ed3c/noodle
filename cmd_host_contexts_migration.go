package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
	"github.com/spf13/cobra"
)

func newHostContextsInspectCmd() *cobra.Command {
	var selected string
	cmd := &cobra.Command{Use: "inspect --id ID", Short: "Read one original control registration and its source version", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := hostDirectory()
			if err != nil {
				return err
			}
			entries, err := loadContextRegistrations(dir)
			if err != nil {
				return err
			}
			entry, ok := entries[selected]
			if !ok {
				return fmt.Errorf("registered context not found")
			}
			choice, err := validateContextRegistration(entry, entry.OriginalHost)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema": 1, "choice": choice, "registration": entry, "effects": []string{}})
		}}
	cmd.Flags().StringVar(&selected, "id", "", "Original registered control ID")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func archiveContextMigration(dir string, before, after contextRegistration, expected string) error {
	if !contextHashPattern.MatchString(expected) || contextVersion(before) != expected {
		return fmt.Errorf("context registration source differs from the expected version")
	}
	if before.ID != after.ID || before.Request != after.Request || before.OriginalHost.HostDir != after.OriginalHost.HostDir {
		return fmt.Errorf("context migration cannot replace the original request, ID or Host directory")
	}
	if _, err := validateContextRegistration(before, before.OriginalHost); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Schema int                 `json:"schema"`
		Kind   string              `json:"kind"`
		Before contextRegistration `json:"before"`
		After  contextRegistration `json:"after"`
	}{1, "context_registration_migration_intent", before, after}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "context-control-migrations", expected+"-"+contextVersion(after)+".json")
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("context migration archive is not a regular file")
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(saved, data) {
			return fmt.Errorf("context migration archive changed")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, data)
}
