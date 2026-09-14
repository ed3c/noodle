---
name: schedule
description: Schedule source-authorized ed3c/noodle Issues from this repository's target backlog. Use only for Noodle schedule cycles in ed3c/noodle.
schedule: "When an authorized ed3c/noodle Issue is ready and no current order owns it"
---

# Target Issue Schedule

Run the target-owned materializer from the repository root:

```bash
go run ./adapters/noodles-github schedule
```

It reads `.noodle/mise.json` and the read-only canonical `.noodle/orders.json`, then writes only `.noodle/orders-next.json`. Noodle owns promotion into canonical orders, worktrees, and process lifecycle. Do not reconstruct or edit the order JSON yourself.

Before selecting a backlog row, derive `owned_ids` from every canonical non-`schedule` order ID. A row is actionable only when its exact `id` is not in `owned_ids`. The transient presence or absence of `.noodle/orders-next.json` is never ownership evidence. If every eligible backlog row is already owned, write `{"orders":[]}`; do not copy canonical orders into the promotion.

Each eligible backlog row is the target adapter's current provider and authorization projection. For one actionable row:

- use its exact `id` as the order ID;
- emit exactly one stage;
- set `do` to the row's exact `execution_skill`;
- set `runtime` to `process`;
- set `prompt` to the JSON encoding of the entire row so the target executor receives its body and authorization binding.

Never infer missing fields, replace the execution skill, or schedule a foreign, malformed, unauthorized, blocked, or already-owned item. Do not add planning, review, cleanup, or follow-up stages. Do not write canonical orders directly.

If no item is actionable, write `{"orders":[]}`.
