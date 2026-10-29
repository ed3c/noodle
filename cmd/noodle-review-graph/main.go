package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/poteto/noodle/internal/reviewgraph"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "review graph failed:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("noodle-review-graph", flag.ContinueOnError)
	root := flags.String("root", ".", "source repository")
	if err := flags.Parse(args); err != nil {
		return err
	}
	graph, err := reviewgraph.Build(context.Background(), *root, flags.Args())
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(graph)
}
