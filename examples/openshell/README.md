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

Before starting the harness, Noodle checks native provider readiness against the
original VM identity and main process. It pins the native observation receipt
and desired configuration while waiting, and refuses withheld, failed or changed
credentials. The query creates native observation metadata; it does not call the
model or rotate credentials. Safe readiness evidence is saved in
`openshell-providers.json` in the local session directory.

For completed-session inspection and the narrowly scoped retained initialization
repair, see the [CLI reference](../../docs/reference/cli.md).

## OpenShell 0.1.5 shutdown limitation

The native gateway does not join background OAuth refresh transactions before
normal shutdown. Noodle's VM stop barrier proves VM completion, but does not
prove that credential refresh has finished. Increasing termination grace does
not resolve this native limitation. A refreshed login is therefore not evidence
that on-demand gateway shutdown is safe for refresh-managed credentials.

Keep this limitation explicit when accepting the subscription route. An
interrupted refresh with an unknown issuer result needs a new dedicated login;
do not replay the saved refresh token or clear its state to retry. Native
shutdown needs to stop admitting refreshes and drain admitted transactions to
provide a complete guarantee. See the pinned native
[shutdown path](https://github.com/NVIDIA/OpenShell/blob/v0.1.5/crates/openshell-server/src/lib.rs#L1013)
and [refresh worker](https://github.com/NVIDIA/OpenShell/blob/v0.1.5/crates/openshell-server/src/provider_refresh.rs#L2229).

## Local task candidate review

`host task review-capabilities` advertises `openshell_local_task_review: 1`
for admitted single-stage `local-task` Codex/Pi candidates. Bound merge and
request-changes require the original runtime's published clean candidate, native
exit success, stop acknowledgement and fresh matching stopped VM evidence.
A missing original owner or changed candidate refuses review. Explicit requeue
starts a new session and retains prior attempt evidence; native session resume
and damaged-candidate merge restoration are not supported for this runtime.
This capability does not establish Soodles input materialization or capture.
