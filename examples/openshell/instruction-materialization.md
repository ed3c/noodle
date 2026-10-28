# Frozen instructions in an OpenShell guest

`DispatchRequest.InstructionBundle` is an explicit `{path, sha256}` reference to
the original owner's immutable manifest. The caller supplies the original order
through `NOODLE_ORDER_ID`. There is no host directory discovery or global skill
name substitution in materialization. The owner remains responsible for listing
the complete selected and supporting skill files.

Manifest version 1 has exactly these fields:

```json
{
  "schema_version": 1,
  "order_id": "original-order",
  "files": [
    {
      "source": {"path": "/owner/frozen/SKILL.md", "sha256": "<lowercase SHA-256>"},
      "original_source": {"path": "/owner/source/SKILL.md", "sha256": "<lowercase SHA-256>"},
      "destination": "skills/selected/SKILL.md",
      "mode": 420
    }
  ],
  "bindings": [
    {"kind": "selected_skill", "name": "selected", "path": "skills/selected/SKILL.md"}
  ]
}
```

`source` names the actual frozen regular file. `original_source` records identity
only; the consumer never reads that original path. Modes are decimal JSON
integers from 0 through 511 (`0777`), not Git mode strings. Bytes and exact modes
are preserved, including binary assets and executable scripts. Missing, changed,
oversized, special, or symlinked source files fail before VM submission. Source
ancestors are checked too. The checks detect observed source changes; they do not
provide an atomic snapshot against arbitrary concurrent host writers.

Limits are 4 MiB for the manifest, 4096 files, 16 MiB per file, and 64 MiB total
file bytes. Unknown fields, duplicate JSON keys, invalid hashes, and a different
order are errors. Exactly one `selected_skill` binding is required. Other kinds
are `support_skill`, `agent`, and `source`. Aliases use Unicode alphanumeric
characters, `-`, or `_`, and are unique under Unicode case folding. Skill entry
bindings end in exact `SKILL.md`; each binding path exactly names a listed file.

Destinations are relative POSIX paths. Traversal, backslashes, colons, Unicode
control characters, leading/trailing segment whitespace, and trailing segment
dots are refused. Case-folded duplicate paths, file/directory ancestor
collisions, and alternate spellings of the same case-folded directory are
refused. The root name `manifest.json` is reserved for the unchanged manifest.

For source, original-source, and destination paths, case-folded segments `.git`,
`.ssh`, `.gnupg`, `.aws`, `.env`, and `.env.*` are forbidden. Leaf names `auth.json`,
`credentials`, `credentials.json`, `credentials.toml`, and suffixes `.pem`/`.key`
are forbidden. This rule does not exclude the entire `.codex` directory.

## Dispatch and execution

The prepared directory is `instructions-<manifest-sha256>`, uploaded under
`/sandbox`, outside `/sandbox/repo`. It does not enter the returned artifact or
candidate commit. `prompt.txt` and frozen files retain their original bytes.
Derived `input.txt` includes `# Materialized task instructions` with typed
binding names, guest paths, and original-source identities. Independent
`instruction-mapping.json` and `spawn.json` retain the mapping, bundle reference,
and reasoning level for later readback.

The profile's optional `instruction_verifier` names an executable under the
existing hash-bound `/sandbox/payload/` tree. A dispatch carrying a manifest
without this executable configured is refused before VM submission. Existing
bare harness dispatches can omit the bundle; that does not certify any external
Soodles instruction materialization.

Build a new Linux verifier in an isolated checkout using the project's pinned
pnpm version, with the architecture selected for the new guest payload:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOMAXPROCS=2 pnpm run build:instructions
```

The result is `bin/noodle-instructions`. Add it only to a new payload/profile
generation; do not change a previously frozen payload. The launch form is:

```text
/sandbox/payload/noodle-instructions --directory /sandbox/instructions-HASH --sha256 HASH --order-id ORDER -- /sandbox/payload/HARNESS [arguments...]
```

The standalone verifier checks the transported manifest and all files, modes,
paths, and extra entries before calling `syscall.Exec`. The same owned native
execution and PID become the true harness. Missing or corrupted material never
reaches that harness. No additional native Exec, daemon, interpreter dependency,
or plugin discovery is introduced. Relative skill references remain intact;
arbitrary hard-coded host paths in scripts are not transparently rewritten.
