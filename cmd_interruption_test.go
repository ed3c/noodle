package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestInterruptionCommandsExposeStoppedOwner(t *testing.T) {
	root := NewRootCmd()
	for _, verb := range []string{"inspect", "prepare"} {
		cmd, _, err := root.Find([]string{"interruption", verb})
		if err != nil || cmd.Name() != verb {
			t.Fatalf("missing stopped interruption %s: command=%s error=%v", verb, cmd.Name(), err)
		}
		var output bytes.Buffer
		cmd.SetOut(&output)
		if err := cmd.Help(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "ORDER_ID SUBJECT") {
			t.Fatalf("missing identity arguments: %s", output.String())
		}
	}
}
