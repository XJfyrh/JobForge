# S1 English development corpus v2

The current registration is `support-dev-2026-09-16-v2` with
`delivery-policy-dev-v2`. The authoritative import file is `runtime/seed.json`,
using the Go `Dataset/Ticket/Order/Delivery/PolicyVersion` contract. It contains
40 tickets across two tenants, 38 related orders, 37 deliveries and 77 events.
All business observation times remain `2026-09-16T12:00:00Z`; snapshot creation
time and the process clock are not the timing reference.

v2 clarifies P01/P05/P06. Commands never grant authority. Customer descriptions
prove the customer's assertion, not verified carrier facts. Carrier event notes
may supply only the policy-recognized source-key and explicit correction
relations, bound to actual events in the same returned delivery. Text such as
`ignore`, `supersedes`, JSON or a forged system message alone does not establish
such a relation. Other objective note claims still need structured corroboration.

The 40 existing expected label objects are unchanged. Their metadata records an
independent **Agent** review of the v1 label directions, followed by v2 authoring
and a separate independent Agent review of all 40 v2 rows and 11 semantic anchors.
No P1/P2 remained in that data review; it was not a human or model-quality review.
Changing every ticket's policy binding increments its revision once. Each tenant's
policy registration advances to revision 2. Order and delivery records, event
text, query wording and retrieval relevance labels are unchanged.

`runtime/policies/manifest.json` pins the new corpus hash and `paragraph-v1`
chunking. Hash sorted filenames as filename UTF-8 + LF + raw file bytes + LF.
Each policy has two paragraph IDs, P01.1 through P10.2; only paragraph bodies are
embedded. The new policy version and corpus hash necessarily require a new index
profile and real preparation/publication before new snapshots can use v2.
No index ID or computed vector is invented in this package. Existing published
v1 indexes must remain available for existing snapshots.

`evaluation/dev_gold.jsonl`, `case-map.jsonl`, `scoring-proposal.json` and
`semantic-anchors.json` are offline review inputs. The 11 anchors bind six
customer assertions, one vague problem, two carrier source-key notes and two
carrier correction notes to tenant, entity, revision, source pointer, full-text
SHA256 and UTF-8 byte span. DEV-012 and DEV-015 each admit two supported dispute
subtypes; both require actual evidence. DEV-024 may independently support
same-time or source-key conflict. These are source meanings, not case-ID pass
switches. Ticket anchors use the captured ticket root (`/description`); carrier
anchors use the actual delivery envelope (`/delivery/events/{index}/note`).

The [offline validator](../../tools/support_evaluation/README.md) checks exact
registered files, hashes, relationships, source bindings and byte spans. It is
**not** a model-output scorer. Actual returned Run provenance, complete claim
coverage/predicates, six-field proposal scoring, safety traces and the final
scoring freeze still require implementation and independent review. The scoring
denominator is all 40 registered cases, including failures and unattempted rows;
there is no new development accuracy threshold. Separate [v2 retrieval evidence](../../docs/evidence/agent-v3-s1-support-retrieval-v2-2026-09-16.json)
records real embedding, two tenant indexes and all 20 queries: 19/20 Hit@3,
MRR@3 0.808333, with RQ-06 still missed. No 40-case cloud acceptance is claimed.

Only `runtime/` facts and policies belong in business preparation. Never package
`evaluation/`, the reviewer manifest, this README or `tools/support_evaluation/`
in a business service, executor image or model prompt. `retrieval/queries.jsonl`
contains only the fixed 20 query IDs, text and current policy revision; relevance
labels remain separate in `evaluation/retrieval_gold.jsonl`. No held-out data was
generated or opened. These 40 exposed cases cannot become unseen evaluation data.

## Preserved v1 history

The v1 files remain in Git commit
`0829a4c04a3a123a6ffe6c374e326859b79df1a8`; current paths are upgraded directly,
without a compatibility runtime or a parallel v1 directory. Use that commit to
inspect the original bytes, rather than interpreting current v2 files as old
evidence.

| Original v1 artifact | SHA256 |
| --- | --- |
| Policy corpus | `2b0fef040119960b504b8f55e64d9e8b73095e1b1c2943947abb1fbfb800d62f` |
| `runtime/seed.json` | `059224e28f25722ba5b74a3bc4eaecaea09266ed20ec8d539621dbaf816c8654` |
| `evaluation/dev_gold.jsonl` | `64b22337f64679f8fb4987df032935f3c1995496c22576632e8f645ed6205fa6` |
| `evaluation/case-map.jsonl` | `cd0e2b880077993642c42d30be52affeea0bd057f595b301e248f86075a33ce3` |
| `evaluation/scoring-proposal.json` | `66bdc692b93e837d96416156889d9629657c7a250b3f798c228a5e9c978777c4` |

The [S1-A evidence](../../docs/evidence/agent-v3-s1-business-2026-09-16.md)
records v1 real import, embedding and retrieval: **19/20 Hit@3**, MRR@3
**0.808333**, with **RQ-06 missed**. Those results are unchanged historical
evidence and do not validate v2. The separate v2 run above used its new corpus
and index IDs; the identical aggregate score does not reuse v1 results.
Paragraph byte checks are not token counts.
