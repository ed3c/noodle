# Runtimes

A runtime is where an agent session executes. Noodle supports local processes, a selected native Codex app-server runtime, and [Sprites](https://sprites.dev) cloud sandboxes. The scheduling agent can pick a runtime per stage, or you can set a project-wide default. Custom runtime plugins are on the roadmap, so you'll be able to define your own.

## Process

The default runtime. Runs agent CLIs as child processes on the host machine.

No setup required. It works out of the box. Best for local development, quick iteration, and small teams where a single machine has enough capacity.

```toml
[runtime]
default = "process"

[runtime.process]
max_concurrent = 4
```

`max_concurrent` defaults to 4. This caps how many local agent processes run simultaneously, independent of the global `max_concurrency` ceiling.

## Native Codex app-server

`codex-app-server` is an experimental foreground runtime. The original Noodle
Host starts the selected Codex binary with `app-server` and owns its stdio
connection through the existing process handle and attempt admission. It does
not attach to a Desktop backend, start a daemon, or fall back to `codex exec`.
The OS observation path currently requires POSIX `ps`.

```toml
[runtime]
default = "codex-app-server"
```

Each attempt retains its native home, foreground process identity, thread and
turn, role, worktree, raw notifications, unsubscribe intent and acknowledgement.
Configured MCP servers retain the original Host configuration. MCP is not the
control channel. Configured capabilities remain subject to Codex policy.

The minimum automatic-release scope is a dedicated attempt owned by this
runtime. Its control path is the native app-server connection owned by Noodle.
Desktop MCP inventory, Session/PID binding, carrier takeover and reconnection
are outside this scope and are not prerequisites for its acceptance. Existing
Desktop resources remain untouched. This does not establish Desktop reclamation.

An attempt that starts MCP servers must still account for those resources when
claiming their release. An attempt without MCP servers has no MCP process-exit
requirement. Neither case requires an MCP control channel. Unknown effects or
unconfirmed exits remain unresolved; reducing scope does not turn them into
release receipts.

Native server requests, including command and file approvals, remain pending
when the Host has no response capability. The runtime saves the original request
without approving, denying or replaying it. Pending requests block new work,
unsubscribe and quota return. Readback and explicit interruption remain available.
Only a native resolution with the same request and thread identity removes the
pending marker. The request and resolution remain in the attempt record.
Resolution alone does not prove a terminal turn, unload or process exit.
There is no new approval UI; an unresolved request requires the original owner
readback or an explicit interruption.

After the original turn completes, the connection stops admitting new work.
Pending or unknown RPC effects block release. The runtime binds child threads
from native activity events and ancestor readback. It checks each child's
parent, worktree and original turn before acting. Normal completion waits for
children and background terminals to finish. An explicit stop interrupts each
known active child through its original thread and turn. Unknown descendants
block release. After each child unloads, the runtime records the current
foreground descendants and unsubscribes its own root thread. It keeps the native
retention delay and waits for unload. No global delay override is installed.

The runtime preserves the raw unsubscribe acknowledgement and validates its
native status. Missing or unknown status blocks release. An acknowledgement
does not return quota. After a complete empty `thread/loaded/list` response,
the runtime saves that original response as `unload_readback` before closing
the foreground process. A failure to save it blocks completion. This receipt
proves the loaded-list observation, not process exit. The runtime
also checks the original terminal, absence of loaded threads, foreground process
exit, stderr drainage, and exit of the recorded descendants. Process observation
uses parent and start identity, not executable names. This bounded observation
does not prove coverage of a process that starts and detaches between snapshots.
For MCP resources started by this attempt, that generation-coverage gap remains
part of acceptance. It is not a requirement to discover all Desktop MCP
generations. Minimum-path acceptance still requires original Host readback of
automatic cleanup, quota return, pause/resume deduplication and same-thread
recovery. An isolated Host acceptance used the actual Codex app-server with a
localhost offline provider. It observed release, quota return, rolling generation
exit, same-thread continuation, retained pause and control replay deduplication.
A native local-task variant also retained candidate review after completion.
The shared production Host completed its initial legacy maintenance on 2026-10-27
(Taipei). The OpenShell task has a restored manual review context and an explicitly
selected current Codex profile. Its original request, environment digest, thread,
worker bytes and review remain bound. Two Medium tasks remain parked with missing
original sources. Host activation does not establish their recovery or acceptance.
The current local readback is recorded in Soodles
`docs/handoffs/2026-10-26-swap-rolling-host/control-noodle-readback.md`.
App SwapManager acceptance still requires the original App carrier's loaded MCP
and Hooks evidence; the Noodle foreground release fixture does not supply it.

A same-role continuation requires the predecessor's release receipt and the
same native home, task and worktree. It resumes the exact original thread.
New threads use the native default history mode. Existing threads retain their
original history mode; this adapter does not migrate their history.
If an interrupt receives an explicit rejection, the runtime saves that refusal
and reads the exact original turn. A terminal readback can continue cleanup;
a nonterminal or changed turn remains unresolved. The runtime does not retry
the interrupt or record a refusal as an acknowledgement.
No archive or unarchive operation is part of this completion path. Native JSON-RPC
records remain verbatim. A restarted Host cannot reattach a lost stdio channel;
recovery retains that attempt for readback without replay or inferred release.
Existing Desktop threads and historical shared-server attempts are not migrated.

## Sprites

Sprites ships as a built-in runtime today but will be split into its own runtime plugin in the future. Cloud sandboxes via [sprites.dev](https://sprites.dev). Each agent runs in an isolated cloud VM with its own filesystem and git worktree.

Use sprites when you want more parallelism than your local machine can handle, or when you want to free up your machine for something else while agents work in the cloud.

```toml
[runtime.sprites]
token_env = "SPRITES_TOKEN"
sprite_name = "my-project"
max_concurrent = 20
```

| Field           | Description                                                |
| --------------- | ---------------------------------------------------------- |
| `token_env`     | Environment variable holding your Sprites API token        |
| `base_url`      | Override the default Sprites API endpoint                   |
| `sprite_name`   | Name of the sprite to use for sessions                     |
| `git_token_env` | Environment variable for git auth (defaults to `GITHUB_TOKEN`) |
| `max_concurrent`| Per-runtime concurrency cap (default 50)                   |

## Choosing a Runtime

| Runtime   | Best for                                | Default `max_concurrent` |
| --------- | --------------------------------------- | -----------------------: |
| process   | Local dev, quick iteration, small teams |                        4 |
| sprites   | Parallel throughput, CI/CD, scaling out |                       50 |

## Configuration

Set the project-wide default runtime:

```toml
[runtime]
default = "process"
```

The scheduling agent can override this per stage by setting the `runtime` field in an order's stage. A common pattern: run fast tasks locally, fan out heavy implementation to sprites.

```json
{
  "stages": [
    {
      "task_key": "execute",
      "prompt": "Implement the new auth flow",
      "runtime": "sprites",
      "status": "pending"
    }
  ]
}
```

Process is always available. Sprites requires a valid token in the configured environment variable. Noodle checks for this at startup and only advertises runtimes that have working credentials.

### Teaching your scheduler to use runtimes

The scheduler reads `routing.available_runtimes` from the mise to know what's available. You teach it how to pick runtimes by writing instructions in your `schedule` skill. Here's an excerpt from a real schedule skill:

```markdown
## Runtime Routing

Read `routing.available_runtimes` from mise before writing orders.

- If only `process` is available, set stage `"runtime": "process"`.
- If `sprites` is available, prefer `"runtime": "sprites"` for
  long-running `execute` work.
- Keep `review`, `reflect`, and `meditate` on `"runtime": "process"`
  unless explicitly justified.
- Always include `"runtime"` on scheduled stages so dispatch routing
  is explicit.
```

Because the scheduler is just a skill, you have full control over how it assigns runtimes. You can tell it to always use sprites for implementation, keep reviews local, or split work across runtimes based on whatever criteria make sense for your project.

## Custom Runtimes

Custom runtime plugins are coming. You'll be able to add your own VM or execution environment by implementing Noodle's runtime interface. If you run your own infrastructure and the built-in runtimes don't fit, this will let you plug it in directly.
