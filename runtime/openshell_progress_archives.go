package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/poteto/noodle/internal/guestprogress"
)

func progressArchiveSources(ctx context.Context, dir string, receipt openShellDispatchReceipt) (map[string]SessionReadbackFileRef, error) {
	return retainedProgressArchiveSources(ctx, dir, receipt)
}

// Only the two original receipt-bound archives use streaming evidence. A role
// name alone never relaxes the generic small-file bound. Cleanup maps retain
// each session prefix, so its dispatch and both archives must share that prefix.
func verifyProgressArchiveSources(refs map[string]SessionReadbackFileRef) (map[string]bool, error) {
	verified := map[string]bool{}
	for key := range refs {
		name := filepath.Base(key)
		if !strings.HasPrefix(name, "progress_archive_") {
			continue
		}
		if name != "progress_archive_noodle-result.tar" && name != "progress_archive_noodle-session.tar" {
			return nil, fmt.Errorf("unknown progress archive evidence role: %s", key)
		}
		prefix := strings.TrimSuffix(key, name)
		if verified[key] {
			continue
		}
		dispatch, ok := refs[prefix+"openshell-dispatch.json"]
		if !ok || filepath.Base(dispatch.Path) != "openshell-dispatch.json" {
			return nil, fmt.Errorf("progress archive original dispatch evidence missing: %s", key)
		}
		var receipt openShellDispatchReceipt
		current, err := ReadSessionReadbackFile(dispatch.Path, &receipt)
		if err != nil {
			return nil, err
		}
		if receipt.Version != 1 || receipt.SessionID != filepath.Base(filepath.Dir(dispatch.Path)) {
			return nil, fmt.Errorf("progress archive session binding differs")
		}
		if current != dispatch {
			return nil, fmt.Errorf("progress archive original dispatch changed")
		}
		source, ok := refs[prefix+"progress_receipt"]
		if !ok || receipt.ProgressSource == nil || source != *receipt.ProgressSource || filepath.Dir(filepath.Dir(source.Path)) != filepath.Dir(dispatch.Path) || filepath.Base(source.Path) != "result.json" || !strings.HasPrefix(filepath.Base(filepath.Dir(source.Path)), "progress-result-") {
			return nil, fmt.Errorf("progress archive original progress receipt missing or outside session")
		}
		var progress guestprogress.Receipt
		progressRef, err := ReadSessionReadbackFile(source.Path, &progress)
		if err != nil {
			return nil, err
		}
		if progressRef != source || !reflect.DeepEqual(receipt.Progress, &progress) {
			return nil, fmt.Errorf("progress archive receipt changed")
		}
		archives, err := progressArchiveSources(context.Background(), filepath.Dir(dispatch.Path), receipt)
		if err != nil {
			return nil, err
		}
		for role, ref := range archives {
			bound, ok := refs[prefix+role]
			if !ok || bound != ref {
				return nil, fmt.Errorf("progress archive original reference differs: %s", prefix+role)
			}
			verified[prefix+role] = true
		}
	}
	return verified, nil
}
