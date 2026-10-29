package runtime

import (
	"context"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/instructionbundle"
)

func (s *openShellSession) uploadInstructions(plan *dispatcher.RemoteHarness) error {
	p := plan.Instructions
	if p == nil {
		return nil
	}
	if _, err := instructionbundle.Verify(context.Background(), p.Directory, p.ManifestSHA256, s.receipt.OrderID); err != nil {
		return err
	}
	if err := s.transfer("upload", p.Directory, "/sandbox"); err != nil {
		return err
	}
	_, err := instructionbundle.Verify(context.Background(), p.Directory, p.ManifestSHA256, s.receipt.OrderID)
	return err
}
