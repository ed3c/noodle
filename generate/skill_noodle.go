// Package generate produces auto-generated files from source metadata.
package generate

import "github.com/poteto/noodle/cmdmeta"

//go:generate go run ./cmd/gen-skill

// GenerateSkillContent returns the full SKILL.md content for the noodle skill.
// The content is static — editorial structure (grouped CLI tables, usage
// examples) doesn't benefit from code generation. The generator exists so CI
// can verify the checked-in snapshot matches this source of truth.
func GenerateSkillContent() (string, error) {
	return skillContent, nil
}

var skillContent = `---
name: noodle
description: >-
  Operate the Noodle CLI — explain commands, find flags, create/edit .noodle.toml config.
  Also covers writing skills for Noodle: the orders pipeline, task-type schedule fields,
  stage composition, and the orders-next.json schema. Use when running noodle commands,
  editing .noodle.toml, writing or updating a skill's schedule field, or authoring new
  task-type skills.
---

# Noodle

Skills that run themselves. Write skills with a ` + "`" + `schedule:` + "`" + ` field describing when they should run. Noodle's scheduler agent reads those descriptions, writes orders, and the loop spawns agents in isolated worktrees to do the work.

## How the Loop Works

1. **Brief** — Noodle gathers project state into ` + "`.noodle/mise.json`" + ` (backlog, active agents, history, capacity, registered skills)
2. **Schedule** — the scheduler agent reads mise, writes ` + "`.noodle/orders-next.json`" + `
3. **Dispatch** — Noodle promotes orders and spawns agent sessions
4. **Execute** — each agent runs in its own worktree with the assigned skill
5. **Merge** — completed work merges back to main

## Skills

A skill is a directory with a ` + "`SKILL.md`" + `. The body is the agent's instructions. The frontmatter is metadata.

**General skills** (no ` + "`schedule:`" + `) are invoked directly by agents. Examples: ` + "`commit`" + `, ` + "`debugging`" + `.

**Scheduled skills** (with ` + "`schedule:`" + `) run autonomously. The scheduler reads the ` + "`schedule:`" + ` value as prose and uses judgment to decide when conditions are met.

` + "```yaml" + `
---
name: quality
description: Post-cook quality gate.
schedule: "Follow-up stage after execute. Cross-provider review preferred."
---
` + "```" + `

Skills live in ` + "`.agents/skills/`" + ` by default. Paths in ` + "`skills.paths`" + ` are searched in order; first match wins.

For the full guide on writing skills, orders, and schedule fields, see [references/skill-authoring.md](references/skill-authoring.md).

## Configuration

Noodle reads ` + "`.noodle.toml`" + ` at project root. Scaffolded on first ` + "`noodle start`" + `. Most projects only need:

` + "```toml" + `
mode = "supervised"  # auto | supervised | manual

[routing.defaults]
provider = "claude"
model = "claude-opus-4-6"

[skills]
paths = [".agents/skills"]
` + "```" + `

Full config reference: https://poteto.github.io/noodle/reference/configuration

## CLI

All commands accept ` + "`--project-dir`" + ` (default: current directory, env: ` + "`NOODLE_PROJECT_DIR`" + `).

### Core

| Command | Description |
|---------|-------------|
| ` + "`noodle start`" + ` | Start the noodle loop |
| ` + "`noodle start --once`" + ` | Run one scheduling cycle and exit |
| ` + "`noodle status`" + ` | Show runtime status (active agents, queue depth, loop state) |
| ` + "`noodle reset`" + ` | Clear all runtime state (refuses if loop is running) |
| ` + "`noodle interruption inspect ORDER_ID SUBJECT`" + ` | Read original interruption custody and successor evidence |
| ` + "`noodle interruption prepare ORDER_ID SUBJECT CUSTODY_SHA256`" + ` | Prepare one exact stopped execution for a successor without dispatching |

### Session discovery and continuation

A new Session needs only the repository or workspace. Use
noodle --project-dir WORKSPACE host status to list all persisted contexts and
known Git worktree snapshots, including gaps. Nothing is selected automatically.
The output is persisted_only with live_verified=false; recorded active state is
not process liveness. selections[].readback_argv carries the selected workspace.
No original Owner, Session or request file is an eligibility condition.
Selected status starts from retained facts and optionally observes a live Host
without starting it. A refusal preserves facts with live_verified=false and a
named gap. Successful exact live replies retain status=ok and live_verified=true.
Use --persisted-only to suppress even the optional live read.

Use --context-id CONTEXT --instance-id INSTANCE after selecting a native target.
Those flags work for host status, detach, recover-unstarted, views bind and
resources residents inspect/acquire/release. A retained --request-file remains
an optional explicit selector. Actual mutations still verify exact targets,
locks, process/effect consistency and idempotence. Discovery never starts Host.

review inspect without arguments reads canonical workspace facts. With
ORDER_ID SUBJECT it also reports selected rejection preflight; missing custody
blocks that mutation, not access to retained facts. host views inspect retains reusable display JSON and detached native readback.
Add --include-workspace for the same inventory in workspace_observation. Display associations
record source relationships and never transfer process control.

### Initial proposal recovery

` + cmdmeta.AdmissionRecoveryGuide + `

| Command | Description |
|---------|-------------|
| noodle admission inspect | Inspect selected proposal, revision, missing facts and exact next argv |
| noodle admission retire PROPOSAL_SHA256 CURRENT_ORDER_REVISION | Archive and retire only the unchanged rejected initial proposal |

### Stopped completed review

` + cmdmeta.StoppedReviewGuide + `

### Already merged publication

The Session verifies recorded provider merge and closure, synchronizes the
selected control checkout, and ensures its loop is stopped before using:

noodle --project-dir PROJECT publication reconcile CLAIM_PATH CLAIM_SHA256 MERGE_HEAD

The retained Noodle publication claim identifies the data target. Reconciliation verifies
session absence, completed writer outcome, unchanged candidate custody and exact
Git ancestry. It preserves failed attempts, cancels only an obsolete pending
merge and records original-order completion. Re-enter the same command after an
interrupted local projection; no merge, scheduler, writer or provider write runs.
This local receipt does not authorize landing or replace provider readback.

### Skills & Schemas

| Command | Description |
|---------|-------------|
| ` + "`noodle skills`" + ` | List resolved skills |
| ` + "`noodle skills list`" + ` | List all resolved skills |
| ` + "`noodle schema <target>`" + ` | Print schema docs for a target |
| ` + "`noodle schema list`" + ` | List available schema targets (` + "`mise`" + `, ` + "`orders`" + `, ` + "`status`" + `) |

### Worktrees

| Command | Description |
|---------|-------------|
| ` + "`noodle worktree create <name>`" + ` | Create a new linked worktree |
| ` + "`noodle worktree create <name> --from <ref>`" + ` | Create from a specific branch or commit |
| ` + "`noodle worktree exec <name> <command...>`" + ` | Run a command inside a worktree (CWD-safe) |
| ` + "`noodle worktree merge <name>`" + ` | Merge a worktree branch into integration branch |
| ` + "`noodle worktree merge <name> --into <branch>`" + ` | Merge into a specific target branch |
| ` + "`noodle worktree list`" + ` | List all worktrees with merge status |
| ` + "`noodle worktree prune`" + ` | Remove merged and patch-equivalent worktrees |
| ` + "`noodle worktree cleanup <name>`" + ` | Remove a worktree without merging |
| ` + "`noodle worktree cleanup <name> --force`" + ` | Remove even with unmerged commits |
| ` + "`noodle worktree hook`" + ` | Run worktree session hook (used internally) |

### Events

| Command | Description |
|---------|-------------|
| ` + "`noodle event emit <type>`" + ` | Emit an external event into the loop |
| ` + "`noodle event emit <type> --payload <json>`" + ` | Emit with a JSON payload |
| ` + "`noodle event emit <type> --session <id>`" + ` | Emit to a specific session's event log |

Full CLI reference: https://poteto.github.io/noodle/reference/cli

## Troubleshooting

1. **"fatal config diagnostics prevent start"** — Check ` + "`.noodle.toml`" + ` against ` + "`noodle schema`" + `.
2. **Missing adapter scripts** — Create scripts or update paths in config.
3. **Stale worktrees** — ` + "`noodle worktree list`" + `, then ` + "`noodle worktree prune`" + `.

## References

- [references/skill-authoring.md](references/skill-authoring.md) — writing skills: pipeline, orders schema, schedule fields, stage composition, full examples
- [references/configuration.md](references/configuration.md) — full .noodle.toml config reference
- https://poteto.github.io/noodle/concepts/adapters — adapter setup, script contracts, provider examples
- https://poteto.github.io/noodle/concepts/scheduling — how the loop schedules and dispatches work
- https://poteto.github.io/noodle/concepts/skills — skill discovery, composition, scheduled vs general
- https://poteto.github.io/noodle/concepts/runtimes — process, sprites, and runtime routing
`
