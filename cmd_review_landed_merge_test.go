package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestLandedLocalMergeCommandsRequireOriginalProofAndExplicitVersion(t *testing.T) {
	root := newReviewCmd(&App{projectDir: t.TempDir()})
	for _, name := range []string{"inspect-landed-local-merge", "reconcile-landed-local-merge"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("missing original stopped command %s: %v", name, err)
		}
		var output bytes.Buffer
		cmd.SetOut(&output)
		if err = cmd.Help(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "PROOF_PATH PROOF_SHA256") {
			t.Fatal("proof identity missing")
		}
		if name == "reconcile-landed-local-merge" {
			if cmd.Flags().Lookup("expected-version") == nil {
				t.Fatal("explicit original version missing")
			}
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{name, "/not-read", "not-read"})
			if err = root.Execute(); err == nil || !strings.Contains(err.Error(), "expected-version") {
				t.Fatalf("missing consent reached repair: %v", err)
			}
		}
	}
}
