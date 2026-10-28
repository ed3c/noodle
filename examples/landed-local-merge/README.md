# Reconcile an already-landed local task

This narrow stopped-owner repair resolves an exact local candidate whose Git
fast-forward landed while canonical completion remained ambiguous. It does not
merge, start a Host, acquire a VM, refresh credentials, replay execution, or make
a replacement Host the original execution owner.

First stop the original context through its authorized owner workflow. Preserve
its original successful OpenShell execution readback and the Soodles capture
that validated it. A later failed readback is not a replacement for that proof.
The helper requires the original context and control-producer locks, one
completed OpenShell attempt/session, its original admitted task configuration,
exact candidate HEAD/tree on the original clean attached integration branch,
and only that attempt's pending merge/dispatch/ack effects. Other sessions,
running or foreign effects, source drift, partial evidence and changed branches
refuse. The first version deliberately does not generalize to multiple attempts.

Prepare a fresh external JSON file with this schema, supplying real FileRefs:

```json
{
  "schema": 1,
  "observation": {"path": "/absolute/original-observation.json", "sha256": "..."},
  "response": {"path": "/absolute/original-native-response.json", "sha256": "..."},
  "observer_intent": {"path": "/absolute/original-capture/intent.json", "sha256": "..."},
  "observer_receipt": {"path": "/absolute/original-capture/receipt.json", "sha256": "..."}
}
```

`observation` is the original Soodles
`local_task_session_readback_observation` with `validation=verified`, exit zero,
and the exact `native_response` saved separately as `response`. The original
Soodles capture's intent and receipt bind the frozen source closure, public task
request, candidate and native response. They are not invented native pre-call
receipts: the original Soodles implementation did not produce such an artifact.
The execution proof must show the original same session, native completion zero,
Stop acknowledgement, observed Stopped phase and the same published candidate.
Only stable non-secret local execution artifacts are reread. Historical custody
and metadata hashes remain in the original readback; no fresh VM observation or
secret custody export is claimed.

Use the new binary with the original control project:

```sh
noodle --project-dir CONTROL review inspect-landed-local-merge PROOF_PATH PROOF_SHA256
noodle --project-dir CONTROL review reconcile-landed-local-merge PROOF_PATH PROOF_SHA256 --expected-version VERSION_FROM_INSPECT
```

Inspection performs no canonical transition. Apply verifies the exact version
again and writes a durable intent containing byte-exact before/after snapshots
under `.noodle/landed-local-merge/<proof-sha256>/`. It retains original attempts,
errors, payloads, event history and acknowledgements. Obsolete pending effects
receive a cancellation **now**, with a separate reconciliation fact; they are
not relabeled as historical successful Git executions. Only the local canonical
order/stage and derived projections become completed.

A complete `receipt.json` is the success boundary. Interruption after intent or
canonical write remains unknown. Another explicit apply may complete that same
intent only when every original input and the current canonical bytes match its
recorded before or after state. Changed snapshots and incomplete/corrupt evidence
refuse. This is not a resume or provider authorization.

Downstream acceptance must consume both the original execution proof and the
completed stopped reconciliation receipt. It must not ask a new Host to attest
the old owner, treat Git HEAD alone as acceptance, or repeat the model call.
