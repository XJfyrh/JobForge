# Offline support development validation

Run from the repository root:

```text
python -m tools.support_evaluation.validate_data
python -m pytest tools/support_evaluation
mypy --explicit-package-bases tools/support_evaluation
```

This module reads only its literal allowlist under `examples/support-agent`.
Reads are bounded to 256 KiB per file; it performs no path discovery, network,
database, model or subprocess work. It does not read or create held-out data.
The reviewer manifest hashes every registered data artifact. Validation covers
UTF-8/JSON, composite identity, all 40 rows, versions, corpus/paragraph limits,
fixed observation time, relationships, unchanged expected labels, 20 query
registrations and all 11 source anchors. Output contains metadata and hashes,
not customer text or model output. No runtime component should import this tool.

`validate_anchor` checks reviewed semantics against a specific document's
identity and bytes. For ticket assertions, its document is the captured ticket
object, matching T's root; for carrier notes it is a delivery evidence envelope.
Delivery note pointers must resolve to the anchor's unique event ID. Correction
targets must actually exist in that delivery, precede the recovered event and
have a critical status. Identical note hashes across events or tenants never
replace full identity checks. Byte spans are half-open `[start, end)` UTF-8
offsets into the complete source field, not character or token positions.

Anchor meaning is an Agent-reviewed annotation, not a keyword classifier.
Closed meanings are customer dispute subtypes, a missing problem description,
a carrier source key or a carrier correction target. Customer assertions do
not prove carrier facts, and note commands grant no authority. Full-field hashes
bind text outside the selected span as well. New text, versions or meanings need
review and a new registration; validation does not invent an equivalent meaning.

The validator's seed-derived document shapes are data QA fixtures. They do not
prove that a source was returned to a Run. A future scorer must first establish
actual tenant/snapshot/policy and evidence-reference provenance, then validate
anchors and every claim's predicate/coverage. It must check the six structured
fields, full 40-case denominator, errors, budget/usage and actual safety traces.
The scoring specification remains pending implementation and final freeze;
these checks do not complete C-05, C-06, C-07 or S1. The v2 policy wording and
anchors passed independent Agent review. Real v2 indexing/retrieval was run
separately, with [recorded results](../../docs/evidence/agent-v3-s1-support-retrieval-v2-2026-09-16.json).
Cloud execution remains unaccepted; this tool makes no model calls.
