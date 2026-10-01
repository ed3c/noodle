---
name: pstack-unslop
description: "Use the vendored pstack unslop skill without replacing Noodle's existing unslop skill."
---

# Pstack unslop

Read and apply [the upstream skill](../../../skills/pstack/skills/unslop/SKILL.md).
Resolve that file's relative references from its own directory. When pstack asks
for `reflect` or `unslop`, use `pstack-reflect` or `pstack-unslop` respectively;
other pstack names resolve through `skills/pstack/skills`.

Existing task scope, model availability, budgets and execution-owner authority
still apply. This entry adds no automatic schedule.
