---
name: pstack
description: "Find and apply the complete vendored pstack skill set in Noodle, with explicit collision mappings and provider limits."
---

# Pstack in Noodle

The complete pinned upstream package is at [skills/pstack](../../../skills/pstack/README.md).
Use the specific skill needed for the task; `poteto-mode` is its optional broad
workflow entry. Do not load every skill or the whole guide as task context.

Noodle resolves names from `.agents/skills` first, then `skills/pstack/skills`.
Two upstream names collide with existing Noodle skills:

| Upstream pstack name | Noodle entry |
| --- | --- |
| reflect | pstack-reflect |
| unslop | pstack-unslop |

All other 45 upstream skill names are available unchanged. Apply this mapping
when following cross-skill references within pstack. Resolve relative links from
the referenced upstream file, not this entry.

Use the current host's actual tool and model capabilities. Cursor slash commands,
Task parameters, rule files and model names in upstream guidance are not proof
that the current provider supports them. Report an unavailable required
capability explicitly; do not invent successful invocation or select an
unapproved replacement runner. Current user scope and budgets take precedence
over upstream multi-model or whole-map workflows.

Installation does not add `schedule:` triggers, enable the dormant Benny
automations, configure credentials or grant merge authority. Noodle's existing
scheduler and admitted execution owners continue to own those operations.
