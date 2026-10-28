---
name: network-proof
description: Record one bounded GET per fixed public host without treating transport errors as policy evidence.
---

Use the bundled `scripts/probe.mjs` through the exact materialized guest path, with `/sandbox/payload/node/bin/node`. Its only argument is the task's fixed stage: `component` or `drawing`.

- Component stage: expected allowed host `example.com`, expected denied host `example.org`.
- Drawing stage: expected allowed host `example.org`, expected denied host `example.com`.

The script sends one HTTPS GET `/` to each host, with no query, body, credentials, custom authorization, redirect following or retries. Each request has a 10-second absolute deadline and 64 KiB body limit. Do not substitute curl, another Node binary, another host, a proxy, or disabled certificate verification.

The script writes `network-proof-<stage>.intent.json` before any request and `network-proof-<stage>.json` afterward. Preserve both files. Never delete or overwrite either to retry. If interrupted after the intent, the outcome is unknown and the script refuses another invocation. An existing finished report can be reread without issuing requests.

A report with `UNKNOWN` is a valid observation artifact, not a failed instruction to repair. DNS, TLS, timeout, reset and other network failures do not prove a denied policy. Even an HTTP response is only transport evidence. The supervising original owner must match these observations with the task's exact native Create policy and original native denial log. Do not change policy or claim R09 passed yourself.

This supporting skill does not replace the selected component/dimensional-drawing methodology. Final HTML remains self-contained static SVG/CSS. Keep probe artifacts separate from visual HTML; no fetch, script, network image or external link belongs in the preview.
