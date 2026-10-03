# Current schedule session recognition

Issue #104 follows the merged interruption owner from Issue #101.
This record describes observations. It grants no runtime or landing authority.

## Fault and owner evidence

The interruption owner rejected original Soodles #229 with
`foreign session schedule-20261003-051323-629f0a`.
The schedule row had no retained attempts. This is normal for its renewable row.

The current `buildSchedulePrompt` producer starts with
`Use Skill(schedule). The selected skill is the single owner of project scheduling and output policy.`
The shared `ReadSessionTarget` consumer recognized only the older
`Use Skill(schedule) to refresh the queue from .noodle/mise.json.` form.
The rejected prompt used the current producer form, not an obsolete format.

The original session's spawn, process, and metadata identified the same session.
Its spawn recorded `skill=schedule`, `runtime=process`, and the original control
checkout as `worktree_path`. Its prompt named that checkout's runtime files.
The same control root's loop event sequence 1 recorded `schedule.completed`
for that session. Its raw log recorded `turn.completed`.
These observations identify a historical scheduler in the original owner.
They do not establish completion of the later interrupted writer.

## Correction and rejection boundary

The shared consumer now accepts the current producer's complete scheduling
header and initial runtime fields. The header skill must match `selected_skill`.
An explicit order identity still wins. The old scheduling form still works.
The producer-to-consumer test calls `buildSchedulePrompt` directly. It does not
copy a historical sample that can drift independently from the producer.

For an extra session absent from canonical attempts, the interruption owner
also checks its spawn session identity, process runtime, skill, and exact
control checkout. The prompt must name the spawn skill and scheduling owner.
The existing admission observer still requires process and process-group absence.

Accepting any `Use Skill` text or a session name prefix would be simpler.
Neither proves that this control checkout spawned a scheduler.
The added spawn checks reject foreign root, runtime, skill, and session values.
Missing spawn evidence and an explicit foreign order also refuse.
The CLI and result schema do not change. This fix creates no attempt or scheduler.

## Recorded verification

Before the fix, the actual producer test failed for both `schedule` and
`project-planner` with `target = "", want "schedule"`.
The historical schedule fixture also failed with `foreign session schedule-prior`.

With canonical macOS TMPDIR, the focused command passed:

```text
go test -race ./loop ./runtime -run 'TestReadSessionTarget|TestInterruption|TestProcessRuntimeRecover' -count=1
ok github.com/poteto/noodle/loop 29.198s
ok github.com/poteto/noodle/runtime 1.413s
```

`go build -o bin/noodle-schedule-target .` succeeded.
It used the existing compiled UI assets as ignored build inputs.
No full test suite, model session, or provider operation ran.

The built binary then ran only `interruption inspect` against the original
Soodles #229 control root. It returned exit 0, `status=recoverable`,
`candidate_unchanged=true`, and an empty `invalid` field in 1.2193 seconds.
The custody digest was
`fd6584c2b80580dd442d06acd334256e3e9e769399851090bf3ad77d1ade6d88`.
This observation does not authorize prepare or later dispatch.

The local raw evidence is under ignored `bin/schedule-session-target-evidence/`.
It contains stdout, stderr, process arguments and exit status, and before/after
file observations. Those observations cover every file under `.noodle`,
tracked and nonignored untracked candidate paths, file modes, the logical Git
index, and candidate HEAD. Both observation files have SHA-256
`1463cbec4c38647af407d0aafd15e371a94348a76f149b22c99c2259f4e68e83`.
The inspect operation left those observed bytes and modes unchanged.
Ignored candidate files were outside this observation. No prepare ran.
