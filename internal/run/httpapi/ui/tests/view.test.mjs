import test from "node:test";
import assert from "node:assert/strict";
import { processingResult, effectInfo, sourceValue, money, matches, claimDescription } from "../view.mjs";

test("an ended execution with an uncertain update remains uncertain", () => {
  assert.equal(processingResult({ state: "failed", outcome: "unknown", proposal_ref: "saved" }), "更新结果待确认");
  assert.equal(processingResult({ state: "cancelled", outcome: "unknown" }), "更新结果待确认");
  assert.equal(effectInfo({ ticket_id: "TK-1", state: "failed" }, { state: "unknown" }).title, "更新结果待确认");
});

test("approval and a bare applied flag cannot fabricate a confirmed receipt", () => {
  const run = { ticket_id: "TK-1", state: "ready", outcome: "approved" };
  assert.equal(effectInfo(run, { state: "none" }).applied, undefined);
  assert.equal(effectInfo({ ...run, outcome: "applied" }, { state: "none" }).title, "更新结果待确认");
  assert.equal(effectInfo(run, { state: "applied" }).title, "更新结果待确认");
  assert.equal(effectInfo(run, { state: "applied", receipt: { ticket_id: "other" } }).applied, undefined);
  assert.equal(effectInfo(run, { state: "applied", receipt: { ticket_id: "TK-1", ticket_status: "escalated" } }).applied, true);
  assert.equal(effectInfo(run, { state: "applied", receipt: { ticket_id: "TK-1" } }, "DEPENDENCY_UNAVAILABLE").applied, undefined);
});

test("late confirmed effect does not rewrite terminal execution facts", () => {
  const run = { ticket_id: "TK-1", state: "failed", outcome: "unknown" };
  const before = structuredClone(run);
  assert.equal(effectInfo(run, { state: "applied", receipt: { ticket_id: "TK-1", ticket_status: "escalated" } }).applied, true);
  assert.deepEqual(run, before);
  assert.equal(processingResult(run), "更新结果待确认");
  const effect = { state: "applied", receipt: { ticket_id: "TK-1", ticket_status: "escalated" } };
  assert.equal(processingResult(run, effect), "结论已保存，工单已更新");
  assert.equal(matches(run, "unknown", effect), false);
  assert.equal(run.state, "failed");
});

test("missing and invalid amounts are not converted to zero", () => {
  for (const value of [undefined, null, -1, 0.5, "0", Number.MAX_SAFE_INTEGER + 1]) assert.equal(money(value), "待确认");
  assert.equal(money(0), "¥ 0.000000");
  assert.equal(money(41200), "¥ 0.041200");
});

test("evidence lookup binds the exact saved reference and business pointer", () => {
  const ref = "business-evidence:one:delivery";
  const steps = [{ output: { evidence_refs: [ref], content: { evidence_ref: ref, delivery: { events: [{ note: "实际物流记录" }] }, snapshot_hash: "internal" } } }];
  assert.equal(sourceValue(steps, ref, "/delivery/events/0/note"), "实际物流记录");
  assert.equal(sourceValue(steps, "business-evidence:other:delivery", "/delivery/events/0/note"), undefined);
  assert.equal(sourceValue(steps, ref, "/delivery/events/1/note"), undefined);
  assert.equal(sourceValue(steps, ref, "/snapshot_hash"), undefined);
  assert.equal(sourceValue(steps, ref, "/__proto__/constructor"), undefined);
});

test("policy bundles resolve the selected paragraph rather than the first match", () => {
  const a = "business-policy:index:first", b = "business-policy:index:second";
  const steps = [{ output: { evidence_refs: [a, b], content: { matches: [{ evidence_ref: a, text: "第一段" }, { evidence_ref: b, text: "第二段" }] } } }];
  assert.equal(sourceValue(steps, b, "/text"), "第二段");
  assert.equal(sourceValue(steps, "business-policy:index:missing", "/text"), undefined);
});

test("unknown server states and unsupported claims stay explicit", () => {
  assert.equal(matches({ state: "unrecognized" }, "processing"), false);
  assert.equal(processingResult({ state: "unrecognized" }), "处理状态待确认");
  assert.equal(processingResult({ state: "constructor" }), "处理状态待确认");
  assert.equal(processingResult({ state: "toString", outcome: "constructor" }), "处理状态待确认");
  assert.equal(claimDescription({ kind: "unrecognized", type: "constructor" }), "该项依据需结合原始业务记录核对。");
  assert.equal(claimDescription({ kind: "unrecognized", type: "INTERNAL_TOKEN" }).includes("INTERNAL_TOKEN"), false);
});
