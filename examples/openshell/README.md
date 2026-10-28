# Managed OpenShell runtime

Use a dedicated gateway whose VM lifecycle belongs exclusively to Noodle.
The Host uses its existing resource budget and resident supervisor. Attaching a
context with this configuration acquires a gateway lease and may start the
service; it does not start a worker or a model request. Each context retains its
own lease, including before its first worker. Only the original gateway owner
can release it after native VM cleanup is confirmed.

```toml
[runtime.openshell]
profile = "/absolute/path/to/openshell-profile.json"
max_concurrent = 1
```

Select `runtime = "openshell"` on the order stage. `runtime.default` controls
requests without an explicit runtime; ordinary cook stages should select their
runtime explicitly. Missing or conflicting profiles refuse admission. OpenShell
never falls back to host execution.

The profile JSON has `schema_version: 1` and these fields:

- `cli`: absolute path or profile-relative path to the installed OpenShell CLI.
- `gateway_name`, `environment`: the explicit CLI gateway name and complete CLI
  environment. Set an explicit absolute `XDG_CONFIG_HOME` for the dedicated
  native client configuration; do not put API
  keys, OAuth tokens, or passwords here.
- `payload_directory`: path to the prepared Linux executable bundle, whose final
  directory name is `payload`. It is uploaded to `/sandbox/payload`.
- `routes`: keys `codex` and/or `pi`, each with `binary` (an absolute guest path
  under `/sandbox/payload/`) and `spec_file` (a native SandboxSpec protobuf JSON
  file). The specification selects one gateway-managed provider, an image,
  canonical main command and task-specific filesystem/network policy.
- `gateway.resident`: the existing ResidentSpec JSON (`name`, `argv`, `directory`,
  `environment`, `sharing: "shared"`, `transport: "network"`). The command must be
  the gateway foreground process, with `force_after_grace: false`.
- `gateway.binding`: `state_root`, `gateway` (loopback HTTPS endpoint), `workspace`
  and persistent `owner` identity. Assign exactly one custody root per gateway.
- `gateway.tls`: `ca_file`, `certificate_file`, `key_file`, `server_name`.
- `gateway.version`, `gateway.compute_driver` and
  `gateway.readiness_timeout_seconds` (1–30).
- `gateway.config_files`: all static native gateway configuration files consumed
  by the foreground command. Include its gateway TOML and imported static files.
  Do not include refreshed provider credential stores; those belong to the
  gateway. File bytes participate in the admitted profile fingerprint.
- `gateway.retained`: explicit historical stopped resources, with `Name`,
  `SandboxID`, and `Workspace`; this is initialization-only evidence, not adoption.

Relative dependency paths resolve against the profile directory. The profile
snapshot binds native policy bytes, static configuration, client TLS material,
and payload content/modes. The selected native client metadata must identify
the same endpoint and mTLS certificate/CA as custody; file transfer checks its
fingerprint and the CLI executable bytes/mode again. Mutable `active_gateway` and `last_sandbox` markers are not
used. Upload checks payload content again. Changing a live
profile cannot silently reconfigure a shared gateway. Stop and inspect the old
contexts before attaching a new configuration.

The guest repository is `/sandbox/repo`; session files live separately under
`/sandbox/noodle-sessions`. Provider secrets remain gateway-managed. Codex uses
its dedicated subscription login, while Pi selects the exact OpenRouter model
from the dispatched task. A stopped VM cannot perform SSH download: collect
outputs first, confirm native stop, then apply validated artifacts to the source
snapshot. Interruption never implies an automatic model retry.
