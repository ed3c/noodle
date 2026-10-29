# Linux review dependency preparation

This is an installation observation, not proof of guest execution. The first
native read-only Poteto task ran on Linux aarch64 and reported that Python and
Git were absent. Its ordinary instructions did not load ub-mode. The selected
review dependencies are therefore prepared separately in the ignored
`runtime/linux-aarch64-python312` directory.

The prepared payload contains CPython 3.12.16 from the official
python-build-standalone 20261026 release, split into two transport files; offline
Python packages and the pinned Crapper, Dryer and Mutator source; and a local
Git binary with PCRE2 and zlib from Ubuntu jammy-updates arm64 packages.
The installation JSON files retain source URLs, upstream checksums and local
file checksums. The Python archive is 29,234,064 bytes. Its upstream SHA-256 is
`bb4b6392670c5d3f93876996fcaa19858a4a413a0c7720144c18b0d6f1f0db2d`.

The offline Python bundle includes coverage 7.14, pytest 9.0.2,
tree-sitter 0.25.2 and tree-sitter-language-pack 1.22.1 with its matching Linux
aarch64 Python parser. The original tool source remains unchanged. These
archives have not yet run inside the selected guest. Local Git helpers for
network access are not provisioned. Java, Clojure and UML Viewer guest execution
remain NOT_RUN. The earlier host installation has a separate evidence boundary.

Nothing in this preparation changes default skills, the shared Host profile,
Poteto's frozen input or normal outer-loop startup. Native review admission,
manifest transfer, bootstrap, analysis, raw result retrieval and recovery still
require their original runtime evidence.


## Role-only tool payload preparation

The new `tool_files` manifest class carries explicitly selected role archives
through the original instruction bundle transport and verifier. Ordinary
instruction limits remain unchanged. The selected role requires the native
`openshell_role_tool_payload=1` capability and a compatible guest verifier.
No default profile or normal Poteto input acquires these archives.

The ignored `runtime/linux-aarch64-review-v2` installation contains the original
CPython archive, Git, Go 1.26.1, Temurin JRE 21.0.13+9, offline Python/Go parsers,
the unchanged tool sources, Clojure 1.12.0 and its two runtime dependency jars.
The Go module archive contains the 31-module closure of the selected review
packages. Offline dependency discovery passed on the Host with Linux/ARM64
selection; it did not execute Linux tests. Official Go and Temurin checksums
matched the downloaded archives. `installation.json` records the exact files.

The UML alias's original main is `uml-viewer.main.ir-generator`. Its pinned
source supports Python but has no Go LanguageGraph. Python IR and Go quality
metrics therefore have separate coverage; Go dependency visualization remains
a gap. No Grok companion or GUI is selected. All guest bootstrap, quality
execution, IR generation and result-return acceptance remain NOT_RUN.


## Go graph role asset

The ignored `runtime/linux-aarch64-review-v3` preparation adds a Linux/ARM64
`noodle-review-graph` executable and the unchanged EDN projection script.
Its installation receipt selects the six pinned v2 dependency archives plus
this seventh archive. The original `InstructionInputs` tool limits accept the
182,694,982-byte selection. No role, candidate or order is admitted by this receipt.

The analyzer was cross-built through the existing `build:review-graph` command
in a source-bound build copy. Its ELF header identifies AArch64; it has not run
inside the selected guest. Analysis, transfer, raw metrics and graph consumption
remain NOT_RUN. The original UML Viewer still has no native Go scanner.
The new archive is selected only through a final-review role's `tool_sources`;
normal Poteto inputs and the default profile remain unchanged.
