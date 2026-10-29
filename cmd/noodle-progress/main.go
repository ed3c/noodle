// Command noodle-progress supervises one guest harness and seals its output
// only after every owned descendant has exited. The host still owns VM Stop.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/poteto/noodle/internal/guestprogress"
)

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "guest progress unavailable:", err)
		os.Exit(74)
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) == 0 {
		return -1, fmt.Errorf("progress operation missing")
	}
	operation := args[0]
	flags := flag.NewFlagSet("noodle-progress", flag.ContinueOnError)
	directory := flags.String("directory", "", "private progress control directory")
	session := flags.String("session-id", "", "original native session")
	command := flags.String("command-sha256", "", "original harness command digest")
	request := flags.String("request-id", "", "one-shot native stop operation UUID")
	repository := flags.String("repository", "", "guest repository")
	sessionDirectory := flags.String("session-directory", "", "guest harness session directory")
	grace := flags.Duration("grace", 10*time.Second, "TERM grace before owned descendants receive KILL")
	if err := flags.Parse(args[1:]); err != nil {
		return -1, err
	}
	token := os.Getenv("NOODLE_PROGRESS_TOKEN")
	tokenHash := sha256.Sum256([]byte(token))
	identity := guestprogress.Identity{SessionID: *session, CommandSHA256: *command, TokenSHA256: hex.EncodeToString(tokenHash[:])}
	switch operation {
	case "run":
		return guestprogress.Run(guestprogress.Config{Directory: *directory, Repository: *repository, SessionDirectory: *sessionDirectory, Identity: identity, Token: token, Argv: flags.Args(), Grace: *grace})
	case "stop":
		if len(flags.Args()) != 0 {
			return -1, fmt.Errorf("stop has unexpected arguments")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		result, err := guestprogress.Stop(ctx, *directory, guestprogress.Request{Schema: 1, Identity: identity, RequestID: *request, Token: token})
		if err != nil {
			return -1, err
		}
		return 0, json.NewEncoder(os.Stdout).Encode(result)
	default:
		return -1, fmt.Errorf("progress operation unsupported: %s", operation)
	}
}
