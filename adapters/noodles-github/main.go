package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: noodles-github receive|sync|handoff|done|refuse")
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

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
