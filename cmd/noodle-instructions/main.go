// Command noodle-instructions verifies an admitted bundle before replacing
// itself with the selected harness. It does not acquire native execution custody.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/instructionbundle"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "instruction verification failed:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("noodle-instructions", flag.ContinueOnError)
	directory := flags.String("directory", "", "materialized bundle directory")
	sha := flags.String("sha256", "", "admitted manifest SHA-256")
	order := flags.String("order-id", "", "original order identity")
	if err := flags.Parse(args); err != nil {
		return err
	}
	argv := flags.Args()
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return fmt.Errorf("absolute harness executable missing")
	}
	if _, err := instructionbundle.Verify(context.Background(), *directory, *sha, *order); err != nil {
		return err
	}
	return replaceProcess(argv)
}
