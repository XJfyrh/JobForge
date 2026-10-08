const el = (id) => document.getElementById(id);
const states = new Set(["ready", "running", "retry_wait", "awaiting_approval", "stopping", "succeeded", "failed", "cancelled"]);
const terminal = new Set(["succeeded", "failed", "cancelled"]);
const names = { on_time: "按期交付", delayed: "交付延迟", disputed: "交付争议", insufficient: "信息不足", conflicting: "事实冲突", record_conclusion: "保存处理结论", request_information: "标记待补充信息", escalate: "标记需人工跟进", open: "待处理", informational_only: "仅供参考", awaiting_information: "等待补充信息", escalated: "需人工跟进", "ticket.order_id": "工单关联订单", "order.delivery_id": "订单关联物流", "delivery.usable_tracking_events": "可用物流事件", "delivery.delivered_event": "签收事件", "ticket.problem_description": "问题描述" };
const stateNames = { ready: "等待处理", running: "正在处理", retry_wait: "等待重试", awaiting_approval: "待人工审批", stopping: "正在停止", succeeded: "处理完成", failed: "处理失败", cancelled: "已取消" };
const descriptions = { none: "尚未生成处理方案", proposal: "方案已保存，请审阅下方内容", approved: "方案已批准，等待核对工单更新结果", applied: "处理结论已保存", rejected: "方案已拒绝，工单未按此方案更新", no_action: "无需更新工单", unknown: "暂未确认更新结果" };
const errors = { UNAUTHORIZED: "凭据失效，已清除当前内容。", FORBIDDEN: "当前身份没有此操作权限。", NOT_FOUND: "当前租户无法访问此 Run。", APPROVAL_CONFLICT: "审批与原方案或已接受决定冲突。", ACTION_CONFLICT: "业务版本已变化，旧方案不能写入。", RESULT_EXPIRED: "受保护内容已过保留期；执行与费用事实仍可查询。", REQUEST_EXPIRED: "原请求已过保留期，旧键不能重新执行。", DEPENDENCY_UNAVAILABLE: "请求未确认，请刷新查询。网络重试不会自动派发新任务。" };
let credential = "", identity = null, epoch = 0, selected = null, cursor = null, loadingList = false, detailVersion = 0, listVersion = 0;
const requests = new Set(), commandKeys = new Map();
const text = (id, value) => { el(id).textContent = value ?? ""; };
const json = (id, value) => text(id, value == null ? "" : JSON.stringify(value, null, 2));
const describe = (value) => descriptions[value] ?? `未知状态：${value ?? "未返回"}`;
const money = (value) => Number.isSafeInteger(value) && value >= 0 ? `${(value / 1000000).toFixed(6)} CNY` : "未知";
const date = (value) => value != null && Number.isFinite(new Date(value).getTime()) ? new Date(value).toLocaleString() : "未记录";
const runFailures = { RUN_DEADLINE_EXCEEDED: "已超过处理期限", MODEL_PROTOCOL_ERROR: "模型响应未通过协议检查", BUDGET_EXHAUSTED: "调用额度已用完" };

function processingResult(run) {
  if (run.state === "failed") {
    const result = run.proposal_ref ? "方案已保存，处理未完成" : "处理失败";
    return runFailures[run.error?.code] ? `${result}：${runFailures[run.error.code]}` : result;
  }
  if (run.state === "cancelled") return run.proposal_ref ? "已取消后续处理，方案已保存" : "已取消后续处理";
  if (run.outcome) return describe(run.outcome);
  if (run.state === "awaiting_approval") return "方案待审批";
  return stateNames[run.state] ?? "处理状态待确认";
}

function clearSession() {
  epoch++;
  for (const controller of requests) controller.abort();
  requests.clear();
  credential = ""; identity = null; selected = null; cursor = null; loadingList = false; detailVersion++; listVersion++;
  commandKeys.clear();
  el("credential").value = ""; el("run-query").value = "";
  el("submit-form").reset(); el("state-filter").value = "";
  el("workspace").hidden = true; el("submit-section").hidden = true; el("detail").hidden = true;
  el("runs").replaceChildren(); el("steps").replaceChildren(); el("run-facts").replaceChildren(); el("run-technical").replaceChildren();
  for (const id of ["proposal-facts", "receipt-facts", "claims"]) el(id).replaceChildren();
  for (const id of ["result", "proposal", "proposal-error", "approval", "approval-context", "effect", "receipt", "cost", "ledger", "steps-error"]) text(id, "");
  for (const id of ["approve", "reject", "cancel", "reconcile", "more"]) el(id).disabled = false;
  text("identity", "尚未连接。凭据只保留在当前页面内存中。");
}

class APIError extends Error {
  constructor(code) { super(code); this.code = code; }
}

async function api(path, body, key) {
  const started = epoch, controller = new AbortController();
  requests.add(controller);
  const timeout = setTimeout(() => controller.abort(), 15000);
  try {
    const headers = { Authorization: `Bearer ${credential}` };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (key) headers["Idempotency-Key"] = key;
    const response = await fetch(path, { method: body === undefined ? "GET" : "POST", headers,
      body: body === undefined ? undefined : JSON.stringify(body), signal: controller.signal, cache: "no-store", credentials: "omit", redirect: "error" });
    if (started !== epoch) throw new APIError("SESSION_CHANGED");
    if (response.status === 401) { clearSession(); throw new APIError("UNAUTHORIZED"); }
    const data = await response.json();
    if (started !== epoch) throw new APIError("SESSION_CHANGED");
    if (!response.ok) throw new APIError(typeof data?.error?.code === "string" ? data.error.code : "INVALID_RESPONSE");
    return data;
  } catch (error) {
    if (started !== epoch && !(error instanceof APIError && error.code === "UNAUTHORIZED")) throw new APIError("SESSION_CHANGED");
    throw error instanceof APIError ? error : new APIError("DEPENDENCY_UNAVAILABLE");
  } finally { clearTimeout(timeout); requests.delete(controller); }
}

function failure(error) {
  if (error?.code === "SESSION_CHANGED") return;
  text("notice", errors[error?.code] ?? `操作未完成：${error?.code ?? "UNKNOWN"}`);
}

async function guard(action) { text("notice", ""); try { await action(); } catch (error) { failure(error); } }

function cell(row, value) { const node = document.createElement("td"); node.textContent = value ?? ""; row.append(node); return node; }

async function list(append = false) {
  if (!identity || append && loadingList) return;
  loadingList = true; const started = epoch, version = ++listVersion;
  el("more").disabled = true;
  try {
    const query = new URLSearchParams({ limit: "20" });
    if (el("state-filter").value) query.set("state", el("state-filter").value);
    if (append && cursor) query.set("cursor", cursor);
    const page = await api(`/v2/runs?${query}`);
    if (version !== listVersion) return;
    if (!Array.isArray(page.items)) throw new APIError("INVALID_RESPONSE");
    if (!append) el("runs").replaceChildren();
    for (const run of page.items) {
      const row = document.createElement("tr"), ticket = cell(row, run.ticket_id), id = document.createElement("small");
      id.textContent = run.run_id; ticket.append(id);
      cell(row, stateNames[run.state] ?? `未知状态：${run.state}`);
      cell(row, processingResult(run)); cell(row, date(run.created_at));
      const button = document.createElement("button"); button.className = "secondary"; button.textContent = "查看";
      button.addEventListener("click", () => guard(() => detail(run.run_id))); cell(row, "").append(button);
      el("runs").append(row);
    }
    cursor = page.next_cursor; el("more").hidden = !cursor;
  } finally { if (started === epoch && version === listVersion) { loadingList = false; el("more").disabled = false; } }
}

async function part(path) {
  try { return { value: await api(path), error: null }; }
  catch (error) { if (error.code === "SESSION_CHANGED" || error.code === "UNAUTHORIZED") throw error; return { value: null, error: error.code }; }
}

async function allSteps(id) {
  const items = []; let after = 0;
  do {
    const page = await api(`/v2/runs/${id}/steps?limit=100&after=${after}`);
    if (!Array.isArray(page.items) || items.length + page.items.length > 32) throw new APIError("INVALID_RESPONSE");
    items.push(...page.items);
    if (page.next_after == null) return items;
    if (!Number.isSafeInteger(page.next_after) || page.next_after <= after) throw new APIError("INVALID_RESPONSE");
    after = page.next_after;
  } while (items.length < 32);
  throw new APIError("INVALID_RESPONSE");
}

function fact(name, value, target = "run-facts") {
  const term = document.createElement("dt"), definition = document.createElement("dd");
  term.textContent = name; definition.textContent = value ?? "未记录"; el(target).append(term, definition);
}

function readableProposal(proposal) {
  el("proposal-facts").replaceChildren(); el("claims").replaceChildren();
  if (!proposal) return;
  for (const [label, field] of [["结论", "conclusion"], ["建议动作", "action"], ["目标工单状态", "target_ticket_status"]]) {
    fact(label, names[proposal[field]] ?? proposal[field] ?? "未提出", "proposal-facts");
  }
  fact("需补充内容", (proposal.requested_fields ?? []).map(value => names[value] ?? value).join("、") || "无", "proposal-facts");
  const references = document.createElement("p");
  references.textContent = `实际来源引用：${(proposal.evidence_refs ?? []).join("\n") || "无"}`;
  el("claims").append(references);
  for (const [index, claim] of (proposal.claims ?? []).entries()) {
    const item = document.createElement("div"), heading = document.createElement("p"), sources = document.createElement("ul");
    const fields = Object.entries(claim).filter(([key]) => key !== "refs").map(([key, value]) => `${key}=${Array.isArray(value) ? value.join(", ") : value}`).join(" · ");
    heading.textContent = `主张 ${index + 1}：${fields}`;
    for (const ref of claim.refs ?? []) { const source = document.createElement("li"); source.textContent = `${ref.evidence_ref} → ${ref.source_pointer}`; sources.append(source); }
    item.append(heading, sources); el("claims").append(item);
  }
}

function readableReceipt(receipt) {
  el("receipt-facts").replaceChildren();
  if (!receipt) return;
  fact("工单", receipt.ticket_id, "receipt-facts"); fact("实际工单状态", names[receipt.ticket_status] ?? receipt.ticket_status, "receipt-facts");
  fact("首次提交时间", Number.isSafeInteger(receipt.applied_at) ? date(receipt.applied_at / 1000) : "未知", "receipt-facts");
}

async function detail(id) {
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id)) throw new APIError("INVALID_ARGUMENT");
  const version = ++detailVersion, started = epoch;
  selected = null; el("detail").hidden = true;
  const run = await api(`/v2/runs/${id}`);
  const [result, approval, effect, calls, actionCalls, steps] = await Promise.all([
    part(`/v2/runs/${id}/result`), part(`/v2/runs/${id}/approval`), part(`/v2/runs/${id}/effect`),
    part(`/v2/runs/${id}/calls`), part(`/v2/runs/${id}/action-calls`),
    allSteps(id).then(value => ({ value, error: null })).catch(error => { if (["SESSION_CHANGED", "UNAUTHORIZED"].includes(error.code)) throw error; return { value: null, error: error.code }; })
  ]);
  if (version !== detailVersion || started !== epoch) return;
  selected = { run, approval: approval.value, effect: effect.value };
  el("detail").hidden = false; el("run-facts").replaceChildren(); el("run-technical").replaceChildren();
  fact("工单", run.ticket_id); fact("处理进度", stateNames[run.state] ?? `未知状态：${run.state}`);
  if (run.error) fact("处理失败原因", `${errors[run.error.code] ?? "处理未完成"} (${run.error.code})`);
  fact("处理期限", date(run.run_deadline));
  for (const [label, value] of [["Run ID", run.run_id], ["租户", run.tenant_id], ["执行状态", run.state], ["首次结果", run.outcome], ["attempt / 恢复", `${run.attempt_no} / ${run.recovery_count}`], ["profile", run.profile_id], ["快照", run.snapshot_id], ["结果引用", result.value?.ref]]) fact(label, value, "run-technical");
  fact("内容保留", run.content_purged_at ? `已于 ${date(run.content_purged_at)} 清理；身份与账本保留` : run.terminal_at ? `终态始于 ${date(run.terminal_at)}，内容尚保留` : "活动内容保留", "run-technical");
  text("result", result.error ? errors[result.error] ?? `结果暂不可用（${result.error}）` : describe(result.value?.disposition));
  const proposalStep = (steps.value ?? []).findLast(step => step.output?.proposal);
  const proposal = approval.value?.proposal ?? proposalStep?.output?.proposal;
  readableProposal(proposal); json("proposal", proposal);
  text("proposal-error", approval.error && approval.error !== "NOT_FOUND" ? `方案查询：${approval.error}` : "");
  const approvalNames = { pending: "等待审批", approved: "方案已批准", rejected: "方案已拒绝" };
  text("approval", approval.error === "NOT_FOUND" ? "尚无待审批方案。" : approval.error ? errors[approval.error] ?? `审批暂不可用（${approval.error}）` : `${approvalNames[approval.value?.status] ?? "尚未决定"}${approval.value?.actor_id ? ` · 审批者：${approval.value.actor_id}` : ""}`);
  const pending = approval.value?.available && run.state === "awaiting_approval";
  el("approval-actions").hidden = identity.role !== "approver" || !pending;
  text("approval-context", pending ? `批准后将保存此方案的处理结论，并把工单标记为「${names[proposal?.target_ticket_status] ?? proposal?.target_ticket_status ?? "方案指定状态"}」。` : "");
  const receipt = effect.value?.receipt;
  text("effect", effect.error ? `暂未确认更新结果（${effect.error}）` : effect.value?.state === "applied" && receipt ? `处理结论已保存，工单已标记为「${names[receipt.ticket_status] ?? receipt.ticket_status}」。` : effect.value?.state === "none" ? "本次任务没有工单更新记录" : describe(effect.value?.state));
  json("receipt", effect.value?.receipt);
  readableReceipt(effect.value?.receipt);
  el("reconcile").hidden = identity.role !== "operator" || !terminal.has(run.state) || !effect.value?.operation_id;
  el("cancel").hidden = identity.role !== "operator" || !states.has(run.state) || terminal.has(run.state);
  const family = run.budget?.family;
  text("cost", `本次业务请求累计：用量费用估算 ${money(family?.known_cost_microyuan)} · 暂占额度 ${money(family?.held_cost_microyuan)} · 费用额度 ${money(family?.limits?.cost_microyuan)}。${family?.frozen === true ? "额度已冻结，需核对费用后才能继续。" : family?.frozen === false ? "额度未冻结。" : "额度状态待确认。"}`);
  json("ledger", { budget: run.budget, calls: calls.error ?? calls.value, action_calls: actionCalls.error ?? actionCalls.value });
  el("steps").replaceChildren(); text("steps-error", steps.error ? `步骤内容不可用：${steps.error}` : "");
  for (const step of steps.value ?? []) {
    const item = document.createElement("details"), summary = document.createElement("summary"), output = document.createElement("pre"), reference = document.createElement("p");
    summary.textContent = `${step.sequence}. ${step.kind} · ${date(step.created_at)}`;
    reference.textContent = `结果引用：${step.output_ref} · 提交 hash：${step.commit_hash}`;
    output.textContent = JSON.stringify(step.output, null, 2); item.append(summary, reference, output); el("steps").append(item);
  }
}

async function command(kind) {
  if (!selected) return;
  const { run, approval } = selected, body = { schema_version: 1 };
  if (kind === "cancel" && !confirm(selected.effect?.operation_id ? "工单更新可能已经开始。取消后续处理后，请核对处理结果。是否继续取消？" : "确认取消此任务的后续处理？")) return;
  let path = `/v2/runs/${run.run_id}/${kind}`;
  if (kind === "approve" || kind === "reject") { path = `/v2/runs/${run.run_id}/approval`; body.decision = kind; body.proposal_hash = approval?.proposal_hash; }
  const binding = `${path}:${JSON.stringify(body)}`;
  if (!commandKeys.has(binding)) commandKeys.set(binding, `ui-${crypto.randomUUID()}`);
  const buttons = ["approve", "reject", "cancel", "reconcile"].map(el), started = epoch, version = detailVersion;
  for (const button of buttons) button.disabled = true;
  try {
    await api(path, body, kind === "reconcile" ? undefined : commandKeys.get(binding));
    text("notice", "操作已接受，正在刷新处理进度和工单更新结果。");
    if (version === detailVersion) await detail(run.run_id);
    await list();
  } finally { if (started === epoch) for (const button of buttons) button.disabled = false; }
}

el("session-form").addEventListener("submit", event => { event.preventDefault(); const key = el("credential").value; clearSession(); credential = key;
  guard(async () => { identity = await api("/v2/identity"); text("identity", `租户：${identity.tenant_id} · 服务端角色：${identity.role} · 审批身份：${identity.actor_id || "未配置"}`);
    el("workspace").hidden = false; el("submit-section").hidden = identity.role !== "operator"; await list(); });
});
el("logout").addEventListener("click", () => { clearSession(); text("notice", "已退出并清除当前内容。"); });
el("submit-form").addEventListener("submit", event => { event.preventDefault(); guard(async () => {
  const body = Object.fromEntries(new FormData(event.target)); body.schema_version = 1; body.run_timeout_seconds = Number(body.run_timeout_seconds);
  const binding = JSON.stringify(body); if (!commandKeys.has(binding)) commandKeys.set(binding, `ui-${crypto.randomUUID()}`);
  const response = await api("/v2/runs", body, commandKeys.get(binding)); await list(); await detail(response.run.run_id);
}); });
el("query-form").addEventListener("submit", event => { event.preventDefault(); guard(() => detail(el("run-query").value)); });
el("state-filter").addEventListener("change", () => guard(() => list()));
el("more").addEventListener("click", () => guard(() => list(true)));
el("refresh").addEventListener("click", () => guard(async () => { await list(); if (selected) await detail(selected.run.run_id); }));
el("reload-detail").addEventListener("click", () => guard(() => selected && detail(selected.run.run_id)));
for (const kind of ["approve", "reject", "cancel", "reconcile"]) el(kind).addEventListener("click", () => guard(() => command(kind)));
