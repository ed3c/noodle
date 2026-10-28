package workspaceartifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// RequireClean is the dispatch gate for work that will become a candidate
// commit. Ignored files remain local; tracked changes and all untracked files
// block dispatch. Status is read without refreshing the on-disk index.
func RequireClean(ctx context.Context, target string) error {
	root, err := repositoryRoot(ctx, target)
	if err != nil {
		return err
	}
	status, err := publicationStatus(ctx, root)
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return fmt.Errorf("candidate worktree has tracked or untracked local changes")
	}
	return nil
}

type publicationReceipt struct {
	Phase               string `json:"phase"`
	Parent              string `json:"parent"`
	ExpectedTree        string `json:"expected_tree"`
	ObservedHead        string `json:"observed_head"`
	OriginalIndexSHA256 string `json:"original_index_sha256"`
	ObservedIndexSHA256 string `json:"observed_index_sha256"`
	Error               string `json:"error"`
}

// Publish creates a candidate commit in the original worktree, without merging.
// Only a clean baseline and an applied, verified return may be published. Once
// Git mutation begins, any failure leaves result unknown and preserves index
// evidence. Publication is one-shot; hooks run normally and are never bypassed.
func Publish(ctx context.Context, target string, baseline *Baseline, result *Result, message string) (candidate string, resultErr error) {
	if baseline == nil || result == nil || !result.Applied || result.State != "applied" {
		return "", fmt.Errorf("artifact publication has no verified applied result")
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("candidate commit message is empty")
	}
	target, err := repositoryRoot(ctx, target)
	if err != nil {
		return "", err
	}
	evidence := filepath.Dir(result.PatchPath)
	if _, err := externalDirectory(target, evidence); err != nil {
		return "", err
	}
	receiptPath := filepath.Join(evidence, "publish.json")
	receipt := publicationReceipt{Phase: "validating", Parent: baseline.Head, OriginalIndexSHA256: baseline.IndexSHA256}
	if err := createPublicationReceipt(receiptPath, receipt); err != nil {
		return "", err
	}
	mutated := false
	indexPath := ""
	defer func() {
		if resultErr != nil {
			receipt.Error = resultErr.Error()
			result.Error = resultErr.Error()
		}
		if mutated {
			observe, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			head, err := git(observe, target, nil, "rev-parse", "--verify", "HEAD")
			cancel()
			if err != nil {
				resultErr = errors.Join(resultErr, err)
			} else {
				receipt.ObservedHead = strings.TrimSpace(string(head))
				if candidate != "" && receipt.ObservedHead != candidate {
					resultErr = errors.Join(resultErr, fmt.Errorf("candidate HEAD changed while retaining publication evidence"))
				}
			}
			index, err := os.ReadFile(indexPath)
			if err != nil {
				resultErr = errors.Join(resultErr, err)
			} else {
				receipt.ObservedIndexSHA256 = digest(index)
				resultErr = errors.Join(resultErr, os.WriteFile(filepath.Join(evidence, "publish-index.after"), index, 0600))
			}
		}
		if resultErr != nil && mutated {
			result.State, result.Applied = "unknown", false
			receipt.Phase = "unknown"
			candidate = ""
		}
		if resultErr != nil {
			receipt.Error = resultErr.Error()
			result.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, writeJSON(receiptPath, receipt))
		if resultErr != nil && mutated {
			result.State, result.Applied = "unknown", false
			candidate = ""
			result.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, writeJSON(filepath.Join(evidence, "result.json"), result))
		if resultErr != nil && mutated {
			result.State, result.Applied = "unknown", false
			candidate = ""
		}
	}()
	head, index, err := identity(ctx, target)
	if err != nil {
		return "", err
	}
	if head != baseline.Head || index != baseline.IndexSHA256 {
		return "", fmt.Errorf("publication target HEAD or index changed since capture")
	}
	before, beforeData, err := scanDirectory(baseline.Directory)
	if err != nil {
		return "", err
	}
	if !reflect.DeepEqual(before, baseline.Entries) {
		return "", fmt.Errorf("publication baseline bytes or modes changed")
	}
	after, afterData, err := scanDirectory(filepath.Join(evidence, "returned"))
	if err != nil {
		return "", err
	}
	returned, _, err := scanDirectory(result.ReturnedDirectory)
	if err != nil {
		return "", err
	}
	if !reflect.DeepEqual(after, returned) {
		return "", fmt.Errorf("returned artifacts changed since apply")
	}
	expected, err := cleanBaselineTree(ctx, target, baseline, beforeData)
	if err != nil {
		return "", err
	}
	changes := changedEntries(before, after)
	if len(changes) == 0 {
		return "", fmt.Errorf("artifact publication has no changes")
	}
	if err := verifyPublicationWork(ctx, target, before, after, changes); err != nil {
		return "", err
	}
	indexPath, err = publicationIndexPath(ctx, target)
	if err != nil {
		return "", err
	}
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		return "", err
	}
	if digest(indexBytes) != baseline.IndexSHA256 {
		return "", fmt.Errorf("publication target index changed during validation")
	}
	if err := os.WriteFile(filepath.Join(evidence, "publish-index.before"), indexBytes, 0600); err != nil {
		return "", err
	}
	receipt.Phase = "mutating"
	result.State, result.Applied = "unknown", false
	if err := writeJSON(receiptPath, receipt); err != nil {
		return "", err
	}
	if err := writeJSON(filepath.Join(evidence, "result.json"), result); err != nil {
		return "", err
	}
	mutated = true
	var staging bytes.Buffer
	for _, name := range names(changes) {
		entry, exists := after[name]
		if !exists {
			fmt.Fprintf(&staging, "0 %s\t%s\x00", strings.Repeat("0", len(baseline.Head)), name)
			delete(expected, name)
			continue
		}
		object, err := git(ctx, target, afterData[name], "hash-object", "-w", "--stdin", "--no-filters")
		if err != nil {
			return "", err
		}
		oid := strings.TrimSpace(string(object))
		expected[name] = publicationTreeEntry{Mode: entry.Mode, Object: oid}
		fmt.Fprintf(&staging, "%s %s\t%s\x00", entry.Mode, oid, name)
	}
	// Blob creation may take time. Reject newly observed changes before staging.
	head, index, err = identity(ctx, target)
	if err != nil {
		return "", err
	}
	if head != baseline.Head || index != baseline.IndexSHA256 {
		return "", fmt.Errorf("publication target HEAD or index changed before staging")
	}
	if err := verifyPublicationWork(ctx, target, before, after, changes); err != nil {
		return "", err
	}
	if _, err := git(ctx, target, staging.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	if err := verifyPublicationIndex(ctx, target, expected); err != nil {
		return "", err
	}
	tree, err := git(ctx, target, nil, "write-tree")
	if err != nil {
		return "", err
	}
	receipt.ExpectedTree = strings.TrimSpace(string(tree))
	receipt.Phase = "committing"
	if err := writeJSON(receiptPath, receipt); err != nil {
		return "", err
	}
	if err := verifyPublicationWork(ctx, target, before, after, changes); err != nil {
		return "", err
	}
	headBytes, err := git(ctx, target, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(headBytes)) != baseline.Head {
		return "", fmt.Errorf("publication target HEAD changed before commit")
	}
	output, commitErr := projectGit(ctx, target, "commit", "-m", message)
	logErr := os.WriteFile(filepath.Join(evidence, "publish-commit.log"), output, 0600)
	if err := errors.Join(commitErr, logErr); err != nil {
		return "", err
	}
	parents, err := git(ctx, target, nil, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(parents))
	if len(fields) != 2 || fields[1] != baseline.Head {
		return "", fmt.Errorf("candidate commit parent differs from original HEAD")
	}
	candidate = fields[0]
	committedTree, err := git(ctx, target, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(committedTree)) != receipt.ExpectedTree {
		return "", fmt.Errorf("candidate commit tree differs from verified artifacts")
	}
	if err := verifyPublicationIndex(ctx, target, expected); err != nil {
		return "", err
	}
	if err := verifyPublicationWork(ctx, target, before, after, changes); err != nil {
		return "", err
	}
	if err := RequireClean(ctx, target); err != nil {
		return "", err
	}
	receipt.Phase, receipt.ObservedHead = "published", candidate
	result.State, result.Applied, result.Error = "published", true, ""
	return candidate, nil
}

func createPublicationReceipt(path string, receipt publicationReceipt) error {
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create one-shot publication evidence: %w", err)
	}
	_, writeErr := file.Write(append(body, '\n'))
	return errors.Join(writeErr, file.Close())
}
