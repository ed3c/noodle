// Package cmdmeta defines CLI command metadata shared between root command
// wiring and the noodle skill generator. This is the single source of truth
// for command names and descriptions.
package cmdmeta

// StoppedReviewGuide is shared by CLI help and the generated skill.
const StoppedReviewGuide = `A Session can discover retained native work with only a repository or workspace:
run noodle --project-dir <workspace> host status or review inspect.
Inspect every candidate and reported gap, then explicitly select ORDER_ID SUBJECT
with review inspect. No original Owner, Session or request file is required.
Read-only observations do not grant mutation eligibility. For an authorized
rejection consume fresh next.argv unchanged. The mutation locks the project,
rechecks exact Git/session custody and process absence, archives the candidate,
applies rejection and reads cleanup back. If preflight is unavailable, the
Session resolves the named missing facts from retained sources without guessing.
Rejected means local non-delivery; it does not close a provider Issue or prove
an outer checkpoint was reconciled. Initial proposals use admission inspect/retire.`

// AdmissionRecoveryGuide is shared by CLI help and the generated skill.
const AdmissionRecoveryGuide = `The Session is responsible for engineering judgment and explicit continuation.
A repository or workspace is sufficient to discover retained candidates with
host status or review inspect; select a target rather than assuming the latest.
Run noodle --project-dir <workspace> admission inspect for admission preflight.
For recoverable, consume fresh next.argv unchanged. Never reconstruct digests
or revisions from memory. For refused, preserve evidence and resolve the named
condition from retained records; no original Owner or Session binding is needed.
Use next.readback_argv after resolving a condition. A valid initial proposal
remains available for normal admission; do not retire it. For retired, follow
next.argv once for canonical readback. For no_proposal, recovery is finished.
A separately admitted intent uses the existing scheduling/admission entry.
Retirement does not dispatch, restart, complete an order or authorize provider
writes. Exact targets, consistency checks, locks and deduplication still apply.`

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
		{Name: "admission", Short: "Inspect or retire a selected rejected initial proposal", Subcommands: []Command{
			{Name: "inspect", Short: "Read admission evidence and exact supported continuation"},
			{Name: "retire", Short: "Retire an unchanged rejected initial proposal"},
		}},
		{Name: "publication", Short: "Bind supervised worktree custody for publication", Subcommands: []Command{
			{Name: "reconcile", Short: "Complete the selected stopped order from an already merged publication"},
			{Name: "claim", Short: "Emit one exact read-only publication claim", Flags: []Flag{
				{Name: "output", Type: "string", Desc: "Fresh absolute path for the claim JSON"},
			}},
		}},
		{Name: "review", Short: "Inspect or reject one exact stopped completed review", Subcommands: []Command{
			{Name: "inspect", Short: "Discover retained review facts and selected mutation preflight"},
			{Name: "reject", Short: "Archive and reject unchanged stopped review custody"},
			{Name: "prepare-local-completion-recovery", Short: "Bind retained terminal local-task work for review recovery"},
			{Name: "prepare-local-merge-recovery", Short: "Bind retained local-task candidate backup bytes"},
		}},
		{Name: "interruption", Short: "Inspect or prepare one stopped interrupted execution", Subcommands: []Command{
			{Name: "inspect", Short: "Read interrupted execution custody and exact continuation"},
			{Name: "prepare", Short: "Preserve unchanged interrupted execution for one successor attempt"},
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
