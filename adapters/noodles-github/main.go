package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/orderx"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: noodles-github receive|sync|schedule|handoff|land|done|refuse")
	}
	if args[0] == "schedule" {
		if len(args) != 1 {
			return fmt.Errorf("schedule accepts no arguments; run: go run ./adapters/noodles-github schedule")
		}
		return scheduleTargetOrder(".")
	}
	policyPath := envOrDefault("NOODLES_GITHUB_POLICY", "policy/github.json")
	capabilitiesPath := envOrDefault("NOODLES_REPO_CAPABILITIES", "policy/repo-capabilities.json")
	policy, err := loadStrictJSON[Policy](policyPath)
	if err != nil {
		return err
	}
	capabilities, err := loadStrictJSON[Capabilities](capabilitiesPath)
	if err != nil {
		return err
	}
	client := NewGitHubClient(envOrDefault("GITHUB_API_URL", "https://api.github.com"), os.Getenv("GITHUB_TOKEN"))
	switch args[0] {
	case "land":
		if len(args) != 1 {
			return fmt.Errorf("land accepts no arguments")
		}
		eventPath := strings.TrimSpace(os.Getenv("GITHUB_EVENT_PATH"))
		if eventPath == "" {
			return fmt.Errorf("GITHUB_EVENT_PATH is required")
		}
		event, err := os.ReadFile(eventPath)
		if err != nil {
			return fmt.Errorf("read GITHUB_EVENT_PATH: %w", err)
		}
		result, err := Land(ctx, client, policy, capabilities, event)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "handoff":
		if len(args) != 2 {
			return fmt.Errorf("handoff requires one target Issue subject")
		}
		result, err := Handoff(ctx, client, policy, capabilities, args[1], strings.TrimSpace(os.Getenv("NOODLES_GITHUB_REMOTE")), ".")
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "receive":
		if len(args) != 1 {
			return fmt.Errorf("receive accepts no arguments")
		}
		eventPath := strings.TrimSpace(os.Getenv("GITHUB_EVENT_PATH"))
		if eventPath == "" {
			return fmt.Errorf("GITHUB_EVENT_PATH is required")
		}
		event, err := os.ReadFile(eventPath)
		if err != nil {
			return fmt.Errorf("read GITHUB_EVENT_PATH: %w", err)
		}
		result, err := Receive(ctx, client, policy, capabilities, event, strings.TrimSpace(os.Getenv("GITHUB_SHA")))
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "sync":
		if len(args) != 1 {
			return fmt.Errorf("sync accepts no arguments")
		}
		items, diagnostics, err := Sync(ctx, client, policy, capabilities)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		for _, item := range items {
			if err := encoder.Encode(item); err != nil {
				return err
			}
		}
		diagnosticEncoder := json.NewEncoder(os.Stderr)
		for _, entry := range diagnostics {
			fmt.Fprint(os.Stderr, "NOODLES_GITHUB_NON_SCHEDULABLE ")
			if err := diagnosticEncoder.Encode(entry); err != nil {
				return err
			}
		}
		return nil
	case "done":
		if len(args) != 2 {
			return fmt.Errorf("done requires one target Issue subject")
		}
		if _, err := parseSubject(args[1]); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "NOODLES_GITHUB_NON_SCHEDULABLE completion is intentionally provider-read-only; Issue closure is outside this adapter")
		return nil
	case "refuse":
		return fmt.Errorf("GitHub backlog mutation is not supported; authorization and Issue closure have separate owners")
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func scheduleTargetOrder(root string) error {
	runtimeDir := filepath.Join(root, ".noodle")
	items, err := readTargetBacklog(filepath.Join(runtimeDir, "mise.json"))
	if err != nil {
		return err
	}
	canonical, err := orderx.ReadOrders(filepath.Join(runtimeDir, "orders.json"))
	if err != nil {
		return err
	}
	owned := make(map[string]struct{}, len(canonical.Orders))
	for _, order := range canonical.Orders {
		if order.ID == "schedule" {
			continue
		}
		if strings.TrimSpace(order.ID) == "" {
			return fmt.Errorf("canonical non-schedule order has an empty id")
		}
		if _, exists := owned[order.ID]; exists {
			return fmt.Errorf("canonical orders contain duplicate id %q", order.ID)
		}
		owned[order.ID] = struct{}{}
	}

	next := orderx.CompactOrdersFile{Orders: []orderx.CompactOrder{}}
	for _, item := range items {
		if _, exists := owned[item.ID]; exists {
			continue
		}
		prompt, err := json.Marshal(item)
		if err != nil {
			return fmt.Errorf("encode target backlog row %q: %w", item.ID, err)
		}
		next.Orders = append(next.Orders, orderx.CompactOrder{
			ID: item.ID, Title: item.Title, Rationale: "target-authorized provider Issue",
			Stages: []orderx.CompactStage{{Do: item.ExecutionSkill, Runtime: "process", Prompt: string(prompt)}},
		})
		break
	}
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode target orders-next: %w", err)
	}
	if _, err := orderx.ParseCompactOrders(data); err != nil {
		return fmt.Errorf("validate target orders-next: %w", err)
	}
	if err := filex.WriteFileAtomic(filepath.Join(runtimeDir, "orders-next.json"), append(data, '\n')); err != nil {
		return fmt.Errorf("write target orders-next: %w", err)
	}
	return nil
}

func readTargetBacklog(path string) ([]BacklogItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read target mise: %w", err)
	}
	var envelope struct {
		Backlog []json.RawMessage `json:"backlog"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode target mise: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, fmt.Errorf("decode target mise trailing data: %w", err)
	}
	items := make([]BacklogItem, 0, len(envelope.Backlog))
	seen := make(map[string]struct{}, len(envelope.Backlog))
	for index, raw := range envelope.Backlog {
		var item BacklogItem
		rowDecoder := json.NewDecoder(bytes.NewReader(raw))
		rowDecoder.DisallowUnknownFields()
		if err := rowDecoder.Decode(&item); err != nil {
			return nil, fmt.Errorf("decode target backlog row %d: %w", index, err)
		}
		if err := validateTargetBacklogItem(item); err != nil {
			return nil, fmt.Errorf("target backlog row %d: %w", index, err)
		}
		if _, exists := seen[item.ID]; exists {
			return nil, fmt.Errorf("target backlog contains duplicate id %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		items = append(items, item)
	}
	return items, nil
}

func validateTargetBacklogItem(item BacklogItem) error {
	number, err := parseSubject(item.ID)
	if err != nil {
		return err
	}
	if item.Repository != targetRepository || item.IssueNumber != number || item.Status != "open" {
		return fmt.Errorf("row identity/status does not match %q", item.ID)
	}
	if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.Body) == "" || item.ExecutionSkill != targetExecutionSkill {
		return fmt.Errorf("row %q is missing title, body, or exact execution skill", item.ID)
	}
	contract, err := parseIssueContract(item.Body, item.IssueNumber)
	if err != nil {
		return fmt.Errorf("row %q Issue contract: %w", item.ID, err)
	}
	authorization := item.Authorization
	if authorization.SchemaVersion != 1 || !sha64Pattern.MatchString(authorization.DispatchIdentity) || strings.TrimSpace(authorization.Sender) == "" {
		return fmt.Errorf("row %q authorization is incomplete", item.ID)
	}
	derived, err := DispatchIdentity(authorization.Declaration)
	if err != nil || derived != authorization.DispatchIdentity {
		return fmt.Errorf("row %q authorization identity is invalid", item.ID)
	}
	declaration := authorization.Declaration
	bodySum := sha256.Sum256([]byte(item.Body))
	if declaration.SourceRepository != sourceRepository || declaration.Target != targetRepository ||
		declaration.Subject != item.ID || declaration.SubjectBodySHA256 != hex.EncodeToString(bodySum[:]) ||
		!sha40Pattern.MatchString(declaration.BaseSHA) || declaration.Runtime != contract.Runtime ||
		declaration.Evidence != contract.Evidence || !equalStrings(declaration.WriteBoundary, contract.WriteBoundary) {
		return fmt.Errorf("row %q authorization does not match the Issue contract", item.ID)
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
