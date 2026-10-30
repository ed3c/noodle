package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
)

type retiredArchiveRecovery struct {
	Before                []byte
	Blocked               string
	Closure               SessionReadbackFileRef
	NativeAbsenceObserved bool
}

// A completed retirement stays authoritative after its task archive disappears.
// The original initialization still audits all native history before new work.
func (c *OpenShellCustody) retiredArchiveBlock() *OpenShellCustodyRecord {
	if c.state.Blocked == "" {
		return nil
	}
	for i := range c.state.Records {
		r := &c.state.Records[i]
		v := r.Retirement
		if v == nil || !v.Completed || !v.Acknowledged {
			continue
		}
		_, missing := readOpenShellClosure(v.Closure, r.Worker.Identity)
		if os.IsNotExist(missing) && missing.Error() == c.state.Blocked {
			return r
		}
	}
	return nil
}

func (c *OpenShellCustody) recoverRetiredArchive(ctx context.Context) error {
	r := c.retiredArchiveBlock()
	if r == nil {
		return nil
	}
	v := r.Retirement
	before, err := os.ReadFile(filepath.Join(c.state.Binding.StateRoot, "owner.json"))
	if err != nil {
		return err
	}
	if err := c.observeRetirement(ctx, r); err != nil {
		return err
	}
	proof, err := json.Marshal(retiredArchiveRecovery{Before: before, Blocked: c.state.Blocked,
		Closure: v.Closure, NativeAbsenceObserved: true})
	if err != nil {
		return err
	}
	path := filepath.Join(c.state.Binding.StateRoot, fmt.Sprintf("retired-archive-recovery-%x.json", sha256.Sum256(before)))
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Equal(existing, proof) {
			return fmt.Errorf("retired archive recovery history differs")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	} else if err := filex.WriteFileAtomicDurable(path, proof); err != nil {
		return err
	}
	blocked := c.state.Blocked
	c.state.Blocked = ""
	if err := c.save(); err != nil {
		c.state.Blocked = blocked
		return err
	}
	return nil
}
