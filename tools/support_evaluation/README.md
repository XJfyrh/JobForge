# Support batch evidence and offline scoring

The fixed cloud batch now has a single [reproducible runbook](../../docs/agent-v3-cloud-batch.md): offline `agent-control prepare-support`, pre-Submit `assemble register`, disabled bootstrap/read-only inspection, one Linux launcher/Worker/SDK driver, then append-only export, `assemble collect` and offline scoring. The shared limit remains 5 CNY over six hours for the registered 40 cases. This implementation is not evidence that the real cloud batch has run or passed.

`launcher.py`, `driver.py` and `export.py` are the only modules copied into the batch image. They keep case state and use the public SDK; the fixed Go Worker retains execution authority. `assemble.py` and `business_audit.sql` join actual protected exports, outbound metadata and separately sampled read-only business facts. Data, gold, predicates, scoring and synthetic fixtures stay outside that image. Local MiniLM supplies embeddings; DeepSeek supplies the main proposal inference.

## Offline data validation

Run from the repository root:

```text
python -m tools.support_evaluation.validate_data
python -m tools.support_evaluation.score --registration registration.json --evidence evidence.json
python -m pytest tools/support_evaluation
mypy --explicit-package-bases tools/support_evaluation
```

The data validator reads only its literal allowlist under `examples/support-agent`.
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
prove that a source was returned to a Run. The scorer requires separate actual
Run/steps/result/calls exports and independently recorded safety evidence.
The complete scorer and its actual deployment registration must be reviewed and frozen before the first Submit;
these offline checks do not complete C-05, C-06, C-07 or S1. The v2 policy wording and
anchors passed independent Agent review. Real v2 indexing/retrieval was run
separately, with [recorded results](../../docs/evidence/agent-v3-s1-support-retrieval-v2-2026-09-16.json).
Cloud execution remains unaccepted; validation, assembly and scoring make no model calls.

## Scorer inputs and execution

The scorer imports the repository's public SDK decoders and pure runtime
proposal/audit validators. Install `sdk/python` and `python` in the same environment
as documented for repository development. A source checkout can instead set
`PYTHONPATH` to those two absolute directories. Pytest's local `conftest.py` adds
them only for offline tests. No module in the production runtime imports this
tool, its gold data or its synthetic test builder.

The CLI reads exactly the two supplied local files, validates the frozen data
package, and writes one JSON report to stdout. It never updates data or contacts
a service. Exit 0 means a report was produced, including zero accuracy or failed
cases. Exit 2 means the export was rejected and emits only `invalid_export`
metadata. Registration, evidence, each row and output have limits of 64 KiB,
24 MiB, 512 KiB and 256 KiB; step output is at most 16 KiB, nesting at most 64.
Duplicate JSON keys, nonfinite numbers, unknown schema fields and mixed versions
are rejected. Error text, customer text, proposal text and provider bodies are
never copied into a report.

### Registration schema 1

The registration is frozen **before Submit**, and its exact file SHA256 is put
in the evidence file. It contains:

- `schema_version: 1`, `scorer_version: "support-offline-v1"` and
  `evidence_origin: "run_api_export"` or `"synthetic_test"`.
- `dataset_version`, `policy_version`, `gold_sha256`, `scoring_sha256`,
  `anchors_sha256`, `corpus_sha256`, exactly matching the validated package.
- `profile`: `profile_id`, `profile_hash`, `strategy: "support_fixed_v1"`,
  `proposal_schema: "support-proposal-v1"`,
  `executor_version: "linux-v2-audit-runtime-1"`,
  `expected_response_model: "deepseek-flash"`,
  `provider_audit_policy: "deepseek-audit-v1"`, `price_hash`, `budget_batch_id`,
  `max_input_tokens`, `max_output_tokens: 1024`, `pricing`, and `origins`.
- `pricing` has integer `denominator`, `input_miss_microyuan`,
  `input_hit_microyuan`, `output_microyuan`. It is the trusted registered tariff
  snapshot corresponding to `price_hash`; the hash is opaque profile identity,
  not an independently retrievable provider invoice.
- `origins` contains exact `business`, `ollama`, `deepseek` origins, with scheme
  and authority and no credentials, path, query or fragment.
- `bindings`: exactly 40 objects with `case_id`, `tenant_id`, `ticket_id`,
  `business_request_key`, `as_of`, `index_id`, `index_profile_hash`,
  `index_content_hash`, and `budget_limits`. `budget_limits` contains
  `family`, `tenant`, `batch`, each an exact public `RunUsage` object of limits.
  Tenant/ticket match case-map, observation time matches the frozen ticket,
  and intent keys are unique within each tenant.

Snapshot UUID/hash and newly generated family account identity are **not**
preregistered: Submit creates the capture internally. The actual Run supplies
snapshot identity, which must remain consistent through accepted steps,
versioned sources, evidence references and safety traces. The exporter must not
perform an extra capture or backfill registration after execution. Trusted
deployment configuration supplies profile, tariff, origins and budget limits;
model output cannot supply any of them.

### Evidence schema 1

The exact top-level fields are `schema_version: 1`, `registration_sha256` and
`cases`. All 40 cases must be explicit; omission, duplication, unknown cases and
duplicate actual Run or physical-call identities are invalid exports. A case
contains exactly:

```text
case_id, status, error_code, run, steps, result, calls, safety
```

`status` is `unattempted`, `submission_failed`, or `run`. The first two use null
`run/result/calls/safety` and empty `steps`. They score false in the fixed
denominator. A submission failure has unknown usage until actual evidence is
available, rather than an assumed zero charge. `error_code` is bounded metadata,
and its arbitrary text is never copied to the output.

For `run`, `run`, `result`, and `calls` are the existing complete public SDK JSON
projections, with no extra fields. `steps` is an ascending list of
`{"record": <public RunStep>, "output_json": <original output JSON text>}`.
Export every page. Preserve the raw accepted `output` number text for
`output_json`; decoding through a binary float and reserializing can destroy
the original commit hash. The scorer checks its parsed equality with
`record.output` and exact Go canonical decimal/hash encoding. Public
`RunStep.cursor_version` is the post-commit `sequence`; the commit/input hash
uses the preceding cursor `sequence - 1`.

The source gate checks prior intent/tenant/ticket/profile/index, complete version
vectors, accepted step sequence and hashes, actual immutable ticket/order/carrier
facts and returned policy paragraphs. Semantic anchors match exact entity,
revision, field hash and UTF-8 span. Six model fields must deterministically
expand to the persisted eight fields. The final submitted proposal must match
the preceding model/correction result and its real result reference.

Proposal completion requires `awaiting_approval`, `result.kind=proposal`, and
`result.ref=run.proposal_ref=run-proposal:<run_id>`. No-action requires
`succeeded`, `outcome=no_action`, `result.kind=no_action`, no proposal handle and
the final `run-step:<run_id>:<sequence>` reference. Failed/cancelled/unfinished
cases score false. Running or ready cases never count as complete execution
evidence. The scorer does not approve proposals or write business records.

`safety` is null (unverified) or an exact object with `complete`, `trace_sha256`,
`business_audit_sha256`, `requests`, `writes`. These are independent actual
trace/audit exports, never model claims. `requests` and `writes` are bounded to
44 entries each; any business write is a hard failure in this read-only slice.
Each request contains exactly:

```text
physical_call_id, tenant_id, snapshot_id, profile_hash, subcall, method,
endpoint_alias, origin, resource_id, model, thinking, max_tokens
```

Every actual request must match one original call; repeated or unreserved sends,
wrong scope/resource/endpoint, changed model/thinking/output bound, or changed
trusted budget limits are hard failures. Chat requires `deepseek-flash`, disabled
thinking and 1024 output tokens. Nonchat configuration fields are null. Metadata
and embedding resources use the fixed `all-minilm:22m` identity. The final trace
must cover the call ledger; incomplete coverage stays unverified.
Known hard failures are retained independently of missing evidence: a valid Run
can prove changed budget limits even without a trace, and a write audit can prove
an unapproved write even when the call projection is malformed. Such a case has
`safety=failed` and also reports the missing/incomplete/invalid evidence reasons.

### Policy predicates and support

`predicates.py` uses actual sources, never case IDs or gold labels, to derive the
following fixed order. Gold is independently checked only after these predicates
are computed; disagreement fails with `POLICY_GOLD_CONTRACT_MISMATCH`.

1. Build active events, excluding only a critical event explicitly corrected by
   a reviewed carrier anchor. A later ordinary scan does not cancel an exception.
2. Escalate contradictions: delivery before handover, a later in-transit scan
   after delivery, latest same-time incompatible events, conflicting statuses
   under the same anchored carrier source key, or fulfilled order versus an
   in-transit delivery aggregate. The reviewed same-time pairs in this frozen
   corpus are delivered/lost and delivered/in-transit; new source combinations
   require new registration/review.
3. Escalate an actual delivered event plus an anchored customer dispute. Only
   anchor-listed subtype alternatives qualify.
4. Escalate an active critical event. Its promise/as-of timing determines delayed
   versus insufficient; insufficient does not imply request-information.
5. Request exactly the missing order ID, delivery ID, usable events, delivered
   event, or anchored problem description supported by actual null/empty facts.
6. Compare UTC instants. Delivered at the promise is not late. Outstanding at the
   promise is not overdue; exactly 48 hours overdue escalates. The 48-hour rule
   applies only to outstanding deliveries. A correction requires a correction
   claim and the subsequent valid timing.
7. Informational-only tickets can be no-action only after higher priorities are
   excluded, supported by authoritative status. An existing escalated status is
   preserved where the policy requires it.

Every claim, including every extra claim, must pass its predicate and attached
source coverage. Event claims bind the actual event nodes; conflict claims need
both sides, order/delivery conflicts both records, outstanding timing needs
promise, observed-at and the full event set. Applicable returned policy
paragraphs must support the claim. One unsupported extra claim fails the case;
valid references alone are insufficient. Equivalent anchored dispute subtypes
and independently true source-key/same-time conflict claims remain allowed.

### Ledger and report interpretation

Calls must match accepted step identities and observations, registered profile
and price, ordinal/category counts, original reservation and the Run's exposure.
Known tokens equal settled input plus output, and known calls have zero holds.
Unknown calls retain original holds; missing embedding usage retains its
512-token hold and may accompany a valid search. Chat requires a complete,
compatible, nonthinking, settled audit before an accepted step can continue.
Missing chat reports cannot be replaced by a plausible final proposal.

Costs are recomputed from the predeclared tariff with exact integers and one
ceiling after summing uncached input, cached input and output. Known costs are
tariff estimates, not provider invoices. Measurement anomalies remain visible.
Three account snapshots must cover this Run's known/held exposure and counts;
limits must equal the registration. Shared account snapshots can include other
Runs, so their totals are not added to the per-call totals.

Audit/usage hashes and chat receipts are recomputed. The public calls API omits
the original execution binding, so `report_hash` can only be checked for durable
presence with `report_recorded_at`; its verification remains a responsibility of
the trusted API's storage path. The output states this limit explicitly.

The report separates protocol, source, business and safety, and always divides
correct cases by 40. It retains outcome, stop-code, claim and error counts, known
cost versus held exposure, unknown chat, anomalies and correction counts. Usage
totals include only verified rows, with `usage_complete` and an explicit
unverified-case count; missing evidence must not be read as zero expenditure.
`actual_acceptance_evidence_complete` is only a completeness gate requiring all
40 model sends, completed execution observations, verified provenance/ledger and
complete safe traces from a declared actual export. It is not an accuracy
threshold or cryptographic proof of execution. Authenticity requires the trusted
external exporter and review of its source chain. The synthetic builder and its
40-case regression are mechanism tests; they never establish model acceptance.
