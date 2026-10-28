# Isolated native acceptance

This runner uses the real OpenShell gateway binary and VM driver, with a new
PKI, empty provider/credential database, state directory, CLI selection and
Noodle resident/custody owner. It never targets a running model VM or the live
gateway. Preparation itself starts neither the gateway nor a VM.

Prepare only during the separately authorized native acceptance window. Use
the reviewed gateway binary/hash and the exact cross-compiled fixture hash.
`--parent` must be a short existing state directory so native Unix socket paths
fit; preparation creates a fresh `ngp-*` child and refuses input reuse.

```sh
python3 examples/openshell/guest-progress/acceptance/prepare.py \
  --parent "$ISOLATED_STATE_PARENT" \
  --gateway "$VERIFIED_GATEWAY_BINARY" \
  --gateway-sha256 "$VERIFIED_GATEWAY_SHA256" \
  --gateway-version '0.1.5+noodle.refresh-drain.1' \
  --cli "$PINNED_OPENSHELL_CLI" \
  --driver-dir "$PINNED_OPENSHELL_LIBEXEC" \
  --fixture "$VERIFIED_LINUX_ARM64_TEST_BINARY"

GOMAXPROCS=2 go run ./examples/openshell/guest-progress/acceptance \
  /absolute/new/ngp-directory/inputs.json
```

The runner records a one-shot invocation before effects. It uses real
`admission.NewSampler` observations, `OpenResidents`, `OpenShellGateway.Acquire`,
`OpenShellCustody.Create` and `StartOwned`. It uploads only the fixed test binary,
checking native same-ID Ready and local source hashes around transfer. The
SandboxSpec has no providers and no network policies. No login, key import,
provider mutation or LLM call is performed.

After an observed fixture exit it requests original VM Stop and waits for a
fresh same-ID Stopped. Only then does it release the worker permit and gateway
lease and join the owned resident. Known fixture failures are retained as FAIL,
not relabeled as a shutdown pass. Unknown create/exec/stop effects keep this
acceptance process, native owner and resource allocation alive; `retained.json`
identifies the original process. Its stdin accepts only `inspect` to display
bounded resources and worker observation. Do not kill it or start a replacement
owner to manufacture successful cleanup.

Evidence is written under the fresh root: preparation command logs, immutable
inputs and source hashes, `run-started.json`, upload log, native fixture log,
exec result, final result or retained diagnostic. Failed Linux fixture data
remains in the VM's `/tmp/ngp-*` path printed in its test log. PKI contents and
private-key bytes are never printed by this runner.

The current source has passed Go compilation/vet and platform-neutral tests.
Native OpenShell execution is pending; neither source inspection nor a plain
Linux test substitutes for this acceptance scope.
