package main

import (
	"context"
	"path/filepath"

	loopruntime "github.com/poteto/noodle/runtime"
)

func prepareGoalTemplatePairing(ctx context.Context, registrationPath, profile, gatewayArtifact, progressArtifact string) (string, error) {
	return loopruntime.PairGoalOpenShellTemplate(ctx, filepath.Join(filepath.Dir(registrationPath), "template-pairings"), profile, gatewayArtifact, progressArtifact)
}
