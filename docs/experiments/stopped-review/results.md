# Stopped-review recovery qualification — Noodle #92

## Scope

Deterministic correction of a missing stopped-owner entry, not a fresh Agent
behavior comparison. Source baseline: `3732a02f9994c23bbb2be30c31801b7a2da586af`.
Product/instruction/test candidate tree before adding this evidence packet:
`785cea3889754cac57c47187e0b553ff4885d282`.
This tree is an identity record, not a separate checkout or admission.

The command owns one clean, completed, supervised pending review satisfying
the existing publication custody binding. It intentionally refuses other
review counts and unsupported/ambiguous custody. It does not repair arbitrary
orders or solve Soodles' separate prepublication checkpoint/config abort.

## Observations

- Baseline CLI: `review inspect` is absent (exit 1). The existing admission
  inspector correctly refuses `soodles-145/0` and returns empty `next.argv`.
- Candidate read-only inspection of the same live original custody returns
  `recoverable` with complete rejection argv. The original snapshot remains
  `5157f13feae428d7d8fbdd5e2742aa844e3ac16336288be34ba62a37a1fefc90`.
  This does not authorize live use of the unlanded candidate.
- Three focused Go test functions cover exact rejection/archive/cleanup,
  refusal cases and three interruption boundaries. The matched race run also
  includes existing admission and publication controls: exit 0, 24.37 seconds.
- A compiler-only planted lock omission makes the unchanged lock refusal
  control fail (exit 1). It proves observer sensitivity only. Product files
  were not rewritten to plant the mutation.
- Native `pnpm check`: exit 0, 96.21 seconds with Node 22.17.1 and an external
  physical temporary directory. Includes build, Go suite, vet, architecture,
  loop fixtures and fixture hashes. Nine architecture warnings name untouched
  existing files; no new warning names the recovery files.
- Generated noodle skill is sourced from `cmdmeta.StoppedReviewGuide`, and
  skill metadata validation passes. This does not prove model behavior.

## Failed attempts retained, not waived

The initial full check exceeded the recorder's 60-second process budget and
was terminated (exit -9). Its result is not a product failure or a passing
acceptance. The next bounded recording used 600 seconds.

The existing publication test compares a raw macOS `/var` fixture path with
the resolved `/private/var` path. Both original baseline and candidate fail
that assertion under the default temporary path. No test was disabled or
changed. A physical temporary path makes the baseline control pass.

The first selected physical path was under `/Users/neon/.codex`, itself a Git
checkout. Its ignored fixture files caused two existing stale-fixture controls
to fail in both baseline and candidate. The final temporary directory is under
`/private/tmp`, outside Git, and the matched baseline controls pass. This is an
explicit runner-condition correction, not a code fix or acceptance exemption.
The default macOS environment limitation remains visible in the raw records.

## Ownership, side effects and limitations

The new entry reuses the existing reducer, projection writer, instance lock,
process observer and worktree cleanup owner. It archives a verified Git bundle
and original snapshot/session bytes before changing custody, and reads cleanup
back instead of interpreting the cleanup helper's warning-only return as proof.
It refuses the competing legacy branch alias before cleanup.

Only fixtures have exercised rejection/removal at this checkpoint. Live #145
was inspected, not rejected. No restart, signal, provider publication, merge,
closure or Soodles checkpoint/config write occurred in recovery qualification.
Noodle #92 filing/contract updates and source-owned dispatch are separate,
user-authorized provider effects. Credentials came from the previously
selected host supplier and were revoked; no credential bytes enter evidence.

No production Noodle child session was launched for this implementation and
no typed child outcome was fabricated. Model context/token costs and a complete
platform transcript are unknown. Raw subprocess records are not a complete
platform transcript. Fault hooks prove the specified process interruption
boundaries, not arbitrary power-loss behavior. Linux exact-head acceptance,
provider landing and live original-owner recovery remain required after this
local qualification; this packet alone does not claim delivery completion.

The timed-out check's stdout ends with a blank line. Its original bytes are
unchanged outside the checkout; the packet stores that one stream losslessly
as `stdout.bin.base64` so repository whitespace checks remain enabled. Decode
base64 and check the `stdout_sha256` in the adjacent original `result.json`.

The original Soodles P-only atom remains separate and unimplemented in its
checkout. There is no P-class efficiency or behavior-improvement claim here.
