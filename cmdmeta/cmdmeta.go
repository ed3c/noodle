// Package cmdmeta defines CLI command metadata shared between root command
// wiring and the noodle skill generator. This is the single source of truth
// for command names and descriptions.
package cmdmeta

// StoppedReviewGuide is the single P-class route for stopped review rejection.
const StoppedReviewGuide = `For an externally authorized rejection of one stopped completed review,
run noodle --project-dir <known-project> review inspect ORDER_ID SUBJECT.
Consume fresh next.argv unchanged; do not infer custody hashes or restart the
scheduler. The owner locks the project, rechecks process absence and exact clean
Git/session custody, archives the candidate, applies the existing rejection and
reads cleanup back. After interruption use inspect before any continuation.
For refused, preserve evidence and report next.required. Rejected means local
non-delivery only: it neither closes a provider Issue nor reconciles an outer
supervisor checkpoint. Initial proposals still use admission inspect/retire.`

// AdmissionRecoveryGuide is shared by CLI help and the generated P-class skill.
const AdmissionRecoveryGuide = `Use the existing Noodle owner with a known project and a stopped loop.
Run noodle --project-dir <known-project> admission inspect and consume its JSON.
For recoverable, execute next.argv exactly as an argv array. Never reconstruct
the digest, revision or command from memory or a historical checkpoint.
For refused, preserve evidence and report next.required to next.provided_by.
After that owner resolves the condition, use next.readback_argv to inspect again.
A valid initial proposal remains available for normal admission; do not retire it.
For retired, follow next.argv once for fresh canonical readback. For no_proposal,
recovery is finished: next.argv is empty and next names the scheduling owner
and inputs for a separately admitted intent. Do not loop inspection indefinitely.
Retirement does not dispatch, restart, complete an order or authorize provider
writes. Record the actual JSON, argv and operation results. Missing owner/project
inputs must be supplied by the supervising operator, not guessed by the Session.`

// Flag describes a CLI flag.
type Flag struct {
	Name    string // long name (e.g. "once")
	Short   string // short name (e.g. "o"), empty if none
	Type    string // "bool", "string", "int", "float64", "[]string"
	Default string // default value as string, empty if zero
	Desc    string // description
}

// Command describes a CLI command or subcommand.
type Command struct {
	Name        string
	Short       string
	Flags       []Flag
	Subcommands []Command
}

// Commands returns the full command tree metadata.
func Commands() []Command {
	return []Command{
		{Name: "start", Short: "Run the scheduling loop", Flags: []Flag{
			{Name: "once", Type: "bool", Desc: "Run one scheduling cycle and exit"},
		}},
		{Name: "version", Short: "Print noodle version"},
		{Name: "status", Short: "Show compact runtime status"},
		{Name: "skills", Short: "List resolved skills", Subcommands: []Command{
			{Name: "list", Short: "List all resolved skills"},
		}},
		{Name: "schema", Short: "Print generated schema docs for Noodle runtime contracts", Subcommands: []Command{
			{Name: "list", Short: "List available schema targets"},
		}},
		{Name: "worktree", Short: "Manage linked git worktrees", Subcommands: []Command{
			{Name: "create", Short: "Create a new linked worktree", Flags: []Flag{
				{Name: "from", Type: "string", Desc: "Branch or commit to base the new worktree on (default: HEAD)"},
			}},
			{Name: "exec", Short: "Run command inside worktree (CWD-safe)"},
			{Name: "merge", Short: "Merge a worktree branch into a target branch", Flags: []Flag{
				{Name: "into", Type: "string", Desc: "Target branch to merge into (default: integration branch)"},
			}},
			{Name: "cleanup", Short: "Remove a worktree without merging", Flags: []Flag{
				{Name: "force", Type: "bool", Desc: "Remove even when unmerged commits exist"},
			}},
			{Name: "list", Short: "List all worktrees with merge status"},
			{Name: "prune", Short: "Remove merged and patch-equivalent worktrees"},
			{Name: "hook", Short: "Run worktree session hook"},
		}},
		{Name: "event", Short: "Manage loop events", Subcommands: []Command{
			{Name: "emit", Short: "Emit an external event", Flags: []Flag{
				{Name: "payload", Type: "string", Desc: "Event payload as JSON"},
			}},
		}},
		{Name: "admission", Short: "Inspect or retire a rejected initial proposal with the stopped owner", Subcommands: []Command{
			{Name: "inspect", Short: "Read admission evidence and exact supported continuation"},
			{Name: "retire", Short: "Retire an unchanged rejected initial proposal"},
		}},
		{Name: "publication", Short: "Bind supervised worktree custody for publication", Subcommands: []Command{
			{Name: "claim", Short: "Emit one exact read-only publication claim", Flags: []Flag{
				{Name: "output", Type: "string", Desc: "Fresh absolute path for the claim JSON"},
			}},
		}},
		{Name: "review", Short: "Inspect or reject one exact stopped completed review", Subcommands: []Command{
			{Name: "inspect", Short: "Read stopped review custody and exact continuation"},
			{Name: "reject", Short: "Archive and reject unchanged stopped review custody"},
		}},
		{Name: "reset", Short: "Clear all runtime state"},
	}
}

// Short returns the Short description for a command by name path.
// For top-level: Short("start"). For sub: Short("plan", "create").
func Short(names ...string) string {
	cmds := Commands()
	for i, name := range names {
		for _, cmd := range cmds {
			if cmd.Name == name {
				if i == len(names)-1 {
					return cmd.Short
				}
				cmds = cmd.Subcommands
				break
			}
		}
	}
	return ""
}
