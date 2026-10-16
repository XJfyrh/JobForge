import { terminal, stateNames, errorText, display, date, money, tone, processingResult, matches, effectInfo, claimDescription, sourceName, sourceValue } from "./view.mjs";

const el = id => document.getElementById(id);
const knownStates = new Set(Object.keys(stateNames));
const roles = { __proto__: null, operator: "业务操作员", approver: "审批人", reader: "只读人员" };
let credential = "", identity = null, epoch = 0, selected = null, cursor = null;
let rows = [], filter = "all", query = "", options = [], loadingList = false;
let listVersion = 0, detailVersion = 0, submitting = false, mutating = false, draftAccepted = false;
let listLoaded = false, listError = false, sourceVersion = 0;
const requests = new Set(), commandKeys = new Map();
const effectsByRun = new Map();
const text = (id, value) => { el(id).textContent = value ?? ""; };
function node(tag, value, className) {
  const item = document.createElement(tag);
  if (value !== undefined) item.textContent = value;
  if (className) item.className = className;
  return item;
}
function notice(value, target = "notice") { text(target, value); el(target).hidden = !value; }
function closeDialogs() { for (const dialog of document.querySelectorAll("dialog[open]")) dialog.close(); }
function busy(id, value) { el(id).disabled = value; el(id).setAttribute("aria-busy", String(value)); }
function clearSession() {
  epoch++;
  for (const controller of requests) controller.abort();
  requests.clear(); commandKeys.clear(); effectsByRun.clear();
  credential = ""; identity = null; selected = null; cursor = null; rows = []; options = [];
  filter = "all"; query = ""; submitting = false; mutating = false; draftAccepted = false;
  listVersion++; detailVersion++; loadingList = false;
  listLoaded = false; listError = false; sourceVersion++;
  closeDialogs();
  el("session-form").reset(); el("submit-form").reset();
  el("search").value = ""; el("run-query").value = "";
  el("processing-option").replaceChildren();
  for (const id of ["runs", "claims", "sources", "receipt-facts", "proposal-facts", "timeline", "approval-meta", "source-content", "confirm-change"]) el(id).replaceChildren();
  for (const id of ["detail-reference", "detail-state", "detail-subtitle", "proposal-title", "proposal-summary", "proposal-ticket", "effect-title", "effect-description", "approval-state", "approval-title", "approval-description", "approval-note", "known-cost", "held-cost", "cost-limit", "budget-state", "detail-created", "account-role", "confirm-title", "confirm-description", "confirm-note", "person-role", "avatar"]) text(id, "");
  for (const id of ["notice", "submit-notice", "proposal-error", "options-note"]) notice("", id);
  for (const id of ["workbench", "detail", "session-info", "new-task"]) el(id).hidden = true;
  el("login").hidden = false;
  busy("connect", false); busy("submit-task", false);
  history.replaceState(null, "", "#tasks");
  window.scrollTo(0, 0);
}
class APIError extends Error { constructor(code) { super(code); this.code = code; } }
async function api(path, body, key) {
  const started = epoch, controller = new AbortController();
  requests.add(controller);
  const timeout = setTimeout(() => controller.abort(), 15000);
  try {
    const headers = { Authorization: "Bearer " + credential };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (key) headers["Idempotency-Key"] = key;
    const response = await fetch(path, { method: body === undefined ? "GET" : "POST", headers,
      body: body === undefined ? undefined : JSON.stringify(body), signal: controller.signal,
      cache: "no-store", credentials: "omit", redirect: "error" });
    if (started !== epoch) throw new APIError("SESSION_CHANGED");
    if (response.status === 401) { clearSession(); throw new APIError("UNAUTHORIZED"); }
    const data = await response.json();
    if (started !== epoch) throw new APIError("SESSION_CHANGED");
    if (!response.ok) throw new APIError(typeof data?.error?.code === "string" ? data.error.code : "INVALID_RESPONSE");
    return data;
  } catch (error) {
    if (started !== epoch && error?.code !== "UNAUTHORIZED") throw new APIError("SESSION_CHANGED");
    throw error instanceof APIError ? error : new APIError("DEPENDENCY_UNAVAILABLE");
  } finally { clearTimeout(timeout); requests.delete(controller); }
}
function failure(error, target = "notice") { if (error?.code !== "SESSION_CHANGED") notice(errorText(error?.code), target); }
async function guard(action) { notice(""); try { await action(); } catch (error) { failure(error); } }
async function part(path) {
  try { return { value: await api(path), error: null }; }
  catch (error) {
    if (["SESSION_CHANGED", "UNAUTHORIZED"].includes(error.code)) throw error;
    return { value: null, error: error.code };
  }
}
function operationKey(binding) {
  if (!commandKeys.has(binding)) commandKeys.set(binding, "ui-" + crypto.randomUUID());
  return commandKeys.get(binding);
}
function showList() {
  if (!identity) return;
  selected = null; detailVersion++;
  el("detail").hidden = true; el("workbench").hidden = false;
  if (location.hash !== "#tasks") history.replaceState(null, "", "#tasks");
  window.scrollTo(0, 0);
}
function setFilter(value) { filter = value; query = ""; el("search").value = ""; showList(); renderRows(); }
function renderRows() {
  const filtered = rows.filter(run => matches(run, filter, effectsByRun.get(run.run_id)) && (run.ticket_id + " " + run.run_id).toLowerCase().includes(query.toLowerCase()));
  el("runs").replaceChildren();
  for (const run of filtered) {
    const row = node("tr"), ticket = node("td");
    ticket.append(node("div", run.ticket_id, "ticket"), node("div", "任务 " + run.run_id.slice(0, 8), "taskno"));
    row.append(ticket);
    const state = node("td"), badge = node("span", stateNames[run.state] ?? "状态待确认", "status " + tone(run)); state.append(badge); row.append(state);
    row.append(node("td", processingResult(run, effectsByRun.get(run.run_id)), "result"), node("td", date(run.created_at), "rowtime"));
    const action = node("button", run.state === "awaiting_approval" ? "查看方案" : matches(run, "unknown", effectsByRun.get(run.run_id)) ? "核对结果" : "查看详情", "row-action");
    action.type = "button"; action.append(icon("arrow-right")); action.addEventListener("click", () => guard(() => detail(run.run_id)));
    const cell = node("td"); cell.append(action); row.append(cell); el("runs").append(row);
  }
  if (!filtered.length) {
    const row = node("tr"), cell = node("td", undefined, "empty"); cell.colSpan = 5;
    cell.append(node("h2", loadingList ? "正在读取任务…" : listError && !listLoaded ? "任务暂时无法读取" : rows.length ? "没有匹配的任务" : "还没有任务"), node("p", loadingList ? "请稍候。" : listError && !listLoaded ? "请刷新列表重试，当前尚未确认任务数量。" : rows.length ? "试试其他工单编号，或清除当前筛选。" : "提交工单后，可以在这里跟进处理进度。"));
    if (rows.length) { const clear = node("button", "清除筛选", "btn"); clear.addEventListener("click", () => setFilter("all")); cell.append(clear); }
    row.append(cell); el("runs").append(row);
  }
  for (const name of ["all", "processing", "awaiting", "ended"]) text("count-" + name, rows.filter(run => matches(run, name, effectsByRun.get(run.run_id))).length);
  text("approval-count", String(rows.filter(run => matches(run, "awaiting")).length).padStart(2, "0"));
  text("unknown-count", String(rows.filter(run => matches(run, "unknown", effectsByRun.get(run.run_id))).length).padStart(2, "0"));
  for (const button of document.querySelectorAll("[data-filter]")) {
    const active = button.dataset.filter === filter;
    button.classList.toggle("active", active); button.setAttribute("aria-selected", String(active));
    button.tabIndex = active || filter === "unknown" && button.dataset.filter === "all" ? 0 : -1;
  }
  text("list-summary", "显示 " + filtered.length + " 个任务，已加载 " + rows.length + " 个");
  if (!listLoaded) {
    for (const name of ["count-all", "count-processing", "count-awaiting", "count-ended", "approval-count", "unknown-count"]) text(name, "—");
    text("list-summary", loadingList ? "正在读取任务…" : "任务数量尚未确认");
  }
}
function icon(name) {
  const image = node("img"); image.src = "/ui/assets/" + name + ".svg"; image.alt = ""; image.className = "icon";
  return image;
}
async function list(append = false) {
  if (!identity || append && (loadingList || !cursor)) return;
  const started = epoch, version = ++listVersion, next = append ? cursor : null;
  loadingList = true; busy("more", true); busy("refresh", true);
  if (!rows.length) renderRows();
  try {
    const params = new URLSearchParams({ limit: "20" });
    if (next) params.set("cursor", next);
    const page = await api("/v2/runs?" + params);
    if (version !== listVersion || started !== epoch) return;
    if (!Array.isArray(page.items) || page.items.some(run => typeof run.run_id !== "string" || typeof run.ticket_id !== "string")) throw new APIError("INVALID_RESPONSE");
    const merged = append ? [...rows, ...page.items] : page.items;
    rows = [...new Map(merged.map(run => [run.run_id, run])).values()];
    listLoaded = true; listError = false;
    cursor = page.next_cursor || null; el("more").hidden = !cursor; renderRows();
  } catch (error) {
    if (started === epoch && version === listVersion) listError = true;
    throw error;
  } finally {
    if (started === epoch && version === listVersion) {
      loadingList = false; busy("more", false); busy("refresh", false);
      if (!rows.length) renderRows();
    }
  }
}
async function allSteps(id) {
  const items = []; let after = 0;
  while (items.length < 32) {
    const page = await api("/v2/runs/" + id + "/steps?limit=100&after=" + after);
    if (!Array.isArray(page.items) || items.length + page.items.length > 32) throw new APIError("INVALID_RESPONSE");
    items.push(...page.items);
    if (page.next_after == null) return items;
    if (!Number.isSafeInteger(page.next_after) || page.next_after <= after) throw new APIError("INVALID_RESPONSE");
    after = page.next_after;
  }
  throw new APIError("INVALID_RESPONSE");
}
async function stepPart(id) {
  try { return { value: await allSteps(id), error: null }; }
  catch (error) {
    if (["SESSION_CHANGED", "UNAUTHORIZED"].includes(error.code)) throw error;
    return { value: null, error: error.code };
  }
}
function labeledValue(target, label, value, className = "change-line") {
  const row = node("div", undefined, className); row.append(node("span", label), node("strong", value)); target.append(row);
}
function record(label, value) {
  const item = node("dl"); item.append(node("dt", label), node("dd", value)); el("receipt-facts").append(item);
}
function timeline(label, time) { const item = node("li"); item.append(node("span", label), node("time", date(time))); el("timeline").append(item); }
async function detail(id, reconciled = false) {
  if (!identity) return;
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id)) throw new APIError("INVALID_ARGUMENT");
  const version = ++detailVersion, started = epoch;
  selected = null;
  for (const control of ["approve", "reject", "cancel", "reconcile", "reload-detail"]) busy(control, true);
  try {
    const run = await api("/v2/runs/" + id);
    const [result, approval, effect] = await Promise.all(["result", "approval", "effect"].map(path => part("/v2/runs/" + id + "/" + path)));
    const steps = run.proposal_ref && !approval.value?.proposal ? await stepPart(id) : { value: null, error: null };
    if (version !== detailVersion || started !== epoch) return;
    const proposal = approval.value?.proposal ?? (steps.value ?? []).findLast(step => step.output?.proposal)?.output.proposal;
    if (!effect.error && effect.value) effectsByRun.set(id, effect.value);
    selected = { run, result, approval, effect, proposal, steps, version, stepRequest: null, reconciled };
    renderRows();
    renderDetail(selected);
    el("workbench").hidden = true; el("detail").hidden = false;
    history.replaceState(null, "", "#task/" + id); window.scrollTo(0, 0);
  } finally { if (version === detailVersion && started === epoch) busy("reload-detail", false); }
}
function renderDetail(target) {
  const { run, proposal, approval, effect, result } = target, info = effectInfo(run, effect.value, effect.error);
  text("detail-reference", "工单 " + run.ticket_id + " / 任务 " + run.run_id.slice(0, 8));
  text("detail-title", info.title ? "处理结果" : proposal ? identity.role === "approver" ? "审阅方案" : "处理方案" : "任务详情");
  text("detail-state", stateNames[run.state] ?? "状态待确认"); el("detail-state").className = "status " + tone(run);
  text("detail-subtitle", run.error ? errorText(run.error.code) : processingResult(run));
  el("result-banner").hidden = !info.title; el("result-banner").className = "result-banner " + info.tone;
  text("effect-title", info.title); text("effect-description", info.description);
  el("receipt-section").hidden = !info.applied; el("receipt-facts").replaceChildren();
  if (info.applied) {
    record("工单编号", effect.value.receipt.ticket_id); record("当前标记", display(effect.value.receipt.ticket_status));
    record("已保存内容", "当前处理结论");
    record("首次更新时间", Number.isSafeInteger(effect.value.receipt.applied_at) ? date(effect.value.receipt.applied_at / 1000) : "暂未记录");
  }
  el("reconcile-note").hidden = !target.reconciled || !info.applied;
  text("proposal-ticket", "工单 " + run.ticket_id); el("proposal-content").hidden = !proposal;
  const title = proposal ? { __proto__: null, escalate: "建议标记为需人工跟进", request_information: "建议补充信息后继续处理", record_conclusion: "建议保存处理结论" }[proposal.action] ?? "请审阅当前处理建议" : run.content_purged_at ? "方案内容已过保留期" : terminal.has(run.state) ? "本次任务没有可展示的处理方案" : "正在准备处理方案";
  text("proposal-title", title);
  text("proposal-summary", proposal?.summary ?? (run.content_purged_at ? "可继续核对工单更新结果与费用记录。" : terminal.has(run.state) ? "请查看处理记录及当前结果。" : "核对工单与相关业务记录后，将在这里展示处理建议。"));
  const proposalError = approval.error && approval.error !== "NOT_FOUND" ? approval.error : target.steps.error ?? (result.error && result.error !== "NOT_FOUND" ? result.error : null);
  notice(proposalError ? errorText(proposalError) : "", "proposal-error");
  el("claims").replaceChildren(); el("sources").replaceChildren(); el("proposal-facts").replaceChildren();
  if (proposal) {
    for (const claim of proposal.claims ?? []) {
      const item = node("div", undefined, "fact-row");
      item.append(node("span", { __proto__: null, timing: "交付时间", dispute: "客户异议", critical: "物流状态", missing: "待补信息", conflict: "记录冲突", correction: "记录修正", ticket_status: "工单状态" }[claim.kind] ?? "事实依据", "fact-label"), node("span", claimDescription(claim), "fact-value"));
      el("claims").append(item);
    }
    if (!(proposal.claims?.length)) el("claims").append(node("p", "当前方案未提供可逐项展示的事实说明。", "proposal-copy"));
    const refs = [...new Set(proposal.evidence_refs ?? [])];
    for (const [index, ref] of refs.entries()) {
      const button = node("button", undefined, "source-card"), heading = node("strong", sourceName(ref) + " " + (index + 1));
      heading.append(icon("arrow-right")); button.append(heading, node("p", "查看该方案保存的实际依据"));
      button.addEventListener("click", () => guard(() => openSource(target, ref))); el("sources").append(button);
    }
    text("changes-title", approval.value?.status === "pending" ? "批准后会发生什么" : "方案中的工单变更");
    labeledValue(el("proposal-facts"), "处理结论", display(proposal.conclusion, "见当前方案"));
    labeledValue(el("proposal-facts"), "工单标记", display(proposal.target_ticket_status, "当前方案未指定"));
    labeledValue(el("proposal-facts"), "处理动作", display(proposal.action, "见当前方案"));
    if (proposal.requested_fields?.length) labeledValue(el("proposal-facts"), "待补充内容", proposal.requested_fields.map(value => display(value, "必要信息")).join("、"));
  }
  renderActions(target, info);
  const family = run.budget?.family;
  text("known-cost", money(family?.known_cost_microyuan)); text("held-cost", money(family?.held_cost_microyuan)); text("cost-limit", money(family?.limits?.cost_microyuan));
  text("budget-state", family?.frozen === true ? "额度已冻结，请核对未确认费用后继续。" : family?.frozen === false ? "额度未冻结。" : "额度状态待确认。");
  text("detail-created", "创建于 " + date(run.created_at));
  el("timeline").replaceChildren(); timeline("任务已提交", run.created_at);
  if (proposal) timeline("处理方案已保存", target.steps.value?.findLast(step => step.output?.proposal)?.created_at);
  if (["approved", "rejected"].includes(approval.value?.status)) timeline(approval.value.status === "approved" ? "方案已批准" : "方案已拒绝", approval.value.decided_at);
  if (info.applied) timeline("工单更新结果已确认", effect.value.observed_at);
  else if (terminal.has(run.state)) timeline(stateNames[run.state], run.terminal_at ?? run.updated_at);
}
function renderActions(target, info) {
  const { run, approval, effect, proposal } = target;
  const pending = approval.value?.available === true && run.state === "awaiting_approval" && !!proposal;
  el("approval-actions").hidden = identity.role !== "approver" || !pending;
  el("cancel").hidden = identity.role !== "operator" || !knownStates.has(run.state) || terminal.has(run.state);
  el("reconcile").hidden = identity.role !== "operator" || !terminal.has(run.state) || !effect.value?.operation_id || info.applied;
  for (const id of ["approve", "reject", "cancel", "reconcile"]) busy(id, mutating);
  let state = "正在处理", title = "正在准备处理方案", description = "方案生成后，可在这里核对建议与依据。", note = "当前不会按未批准的方案更新工单。", status = "progress";
  if (info.applied) { state = "已确认更新"; title = "处理结论已保存"; description = "工单已标记为「" + display(effect.value.receipt.ticket_status) + "」。"; note = "后续需由团队继续处理此工单。"; status = "done"; }
  else if (pending) { state = identity.role === "approver" ? "待你审批" : "等待人工审批"; title = identity.role === "approver" ? "确认后更新工单" : "方案已准备好"; description = "请核对方案与依据。批准后将保存处理结论，并更新工单标记。"; note = identity.role === "approver" ? "批准适用于当前展示的方案。" : "审批由独立审批人完成。"; status = "wait"; }
  else if (run.state === "awaiting_approval" && proposal) { state = "方案已保存"; title = "当前方案暂无法审批"; description = "请刷新核对方案是否仍在有效期内。"; note = "当前不会提交审批决定。"; status = "wait"; }
  else if (info.title === "更新结果待确认") { state = "结果待确认"; title = "核对已有更新记录"; description = info.description; note = "仅查询已有记录，不会重新提交任务或再次更新工单。"; status = "unknown"; }
  else if (approval.value?.status === "approved") { state = "方案已批准"; title = terminal.has(run.state) ? "请核对处理结果" : "等待确认工单更新"; description = "批准决定已保存，实际更新结果尚未确认。"; note = "批准决定与实际工单更新分别记录。"; }
  else if (terminal.has(run.state)) { state = stateNames[run.state]; title = "本次任务已结束"; description = info.description || processingResult(run); note = "请以工单更新记录确认实际效果。"; status = "closed"; }
  text("approval-state", state); el("approval-state").className = "status " + status;
  text("approval-title", title); text("approval-description", description); text("approval-note", note);
  el("approval-meta").replaceChildren();
  if (pending) labeledValue(el("approval-meta"), "审批有效期至", date(approval.value.permission_expires_at));
  if (proposal) labeledValue(el("approval-meta"), "方案中的工单标记", display(proposal.target_ticket_status));
  labeledValue(el("approval-meta"), "任务处理期限", date(run.run_deadline));
}
async function openSource(target, ref) {
  if (selected !== target) return;
  const version = ++sourceVersion;
  text("source-title", sourceName(ref)); el("source-content").replaceChildren(node("p", "正在读取保存的依据…"));
  el("source-dialog").showModal();
  if (target.steps.value === null && !target.steps.error) {
    if (!target.stepRequest) target.stepRequest = stepPart(target.run.run_id);
    const steps = await target.stepRequest;
    if (selected !== target || target.version !== detailVersion || version !== sourceVersion || !el("source-dialog").open) return;
    target.steps = steps;
  }
  if (selected !== target || version !== sourceVersion || !el("source-dialog").open) return;
  el("source-content").replaceChildren();
  if (target.steps.error) { el("source-content").append(node("p", errorText(target.steps.error))); return; }
  const pointers = [...new Set((target.proposal?.claims ?? []).flatMap(claim => claim.refs ?? []).filter(item => item.evidence_ref === ref).map(item => item.source_pointer))];
  let shown = 0;
  for (const pointer of pointers) {
    const value = sourceValue(target.steps.value ?? [], ref, pointer);
    if (value === undefined) continue;
    const valueText = typeof value === "boolean" ? value ? "是" : "否" : display(value, String(value));
    el("source-content").append(node("p", valueText)); shown++;
  }
  if (!shown) el("source-content").append(node("p", "已保存该来源引用，但当前没有可核对的业务片段。请联系处理团队核查原始记录。"));
  el("source-content").append(node("p", "以上内容来自当前方案保存的实际业务记录。", "tiny"));
}
function confirmation(target, kind) {
  const approve = kind === "approve", reject = kind === "reject";
  text("confirm-title", approve ? "确认批准当前方案？" : reject ? "确认拒绝此方案？" : "取消后续处理？");
  text("confirm-description", "工单 " + target.run.ticket_id + " · 任务 " + target.run.run_id.slice(0, 8));
  el("confirm-change").replaceChildren();
  if (approve) {
    labeledValue(el("confirm-change"), "工单标记", display(target.proposal?.target_ticket_status));
    labeledValue(el("confirm-change"), "处理动作", display(target.proposal?.action));
    el("confirm-change").append(node("p", target.proposal?.summary ?? ""));
  }
  text("confirm-note", approve ? "批准后开始更新，实际结果将在任务详情中确认。" : reject ? "拒绝后，本次任务结束，工单不会按此方案更新。" : "后续步骤将停止。已发出的更新仍需核对，已发生的更新不会撤回。");
  text("confirm-action", approve ? "确认批准" : reject ? "确认拒绝" : "确认取消");
  el("confirm-action").className = "btn " + (approve ? "primary" : "danger");
  const dialog = el("confirm-dialog"); dialog.returnValue = ""; dialog.showModal();
  return new Promise(resolve => {
    const accept = () => dialog.close("accepted");
    el("confirm-action").addEventListener("click", accept, { once: true });
    dialog.addEventListener("close", () => { el("confirm-action").removeEventListener("click", accept); resolve(dialog.returnValue === "accepted"); }, { once: true });
  });
}
async function command(kind) {
  const target = selected;
  if (!target || mutating) return;
  if (["approve", "reject"].includes(kind) && (identity.role !== "approver" || !target.approval.value?.available || target.run.state !== "awaiting_approval")) return;
  if (["cancel", "reconcile"].includes(kind) && identity.role !== "operator") return;
  if (kind !== "reconcile" && !await confirmation(target, kind)) return;
  if (selected !== target || target.version !== detailVersion) return;
  const started = epoch, body = { schema_version: 1 }, id = target.run.run_id;
  let path = "/v2/runs/" + id + "/" + kind;
  if (["approve", "reject"].includes(kind)) { path = "/v2/runs/" + id + "/approval"; body.decision = kind; body.proposal_hash = target.approval.value.proposal_hash; }
  mutating = true; renderActions(target, effectInfo(target.run, target.effect.value, target.effect.error));
  try {
    await api(path, body, kind === "reconcile" ? undefined : operationKey(path + ":" + JSON.stringify(body)));
    notice(kind === "reconcile" ? "已核对已有更新记录。" : "操作已接受，请核对最新处理进度和工单更新结果。");
    if (selected === target) await detail(id, kind === "reconcile");
    await list();
  } finally {
    if (started === epoch) { mutating = false; if (selected) renderActions(selected, effectInfo(selected.run, selected.effect.value, selected.effect.error)); }
  }
}
function newTask() {
  if (identity?.role !== "operator" || !options.length || submitting) return;
  if (draftAccepted) { el("submit-form").reset(); draftAccepted = false; }
  if (!el("request-key").value) el("request-key").value = "request-" + crypto.randomUUID();
  notice("", "submit-notice"); el("create-dialog").showModal(); el("ticket-input").focus();
}
async function submit(event) {
  event.preventDefault();
  if (submitting || identity?.role !== "operator") return;
  const option = options[Number(el("processing-option").value)], ticket = el("ticket-input").value.trim(), requestKey = el("request-key").value.trim();
  if (!option || !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(ticket) || !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(requestKey)) {
    notice("工单编号与业务请求编号须为 1–128 位字母、数字或 . _ : -，且以字母或数字开头。", "submit-notice"); return;
  }
  const body = { schema_version: 1, ticket_id: ticket, business_request_key: requestKey,
    profile_id: option.profile_id, budget_batch_id: option.budget_batch_id, run_timeout_seconds: Number(el("timeout").value) * 60 };
  if (!Number.isSafeInteger(body.run_timeout_seconds) || body.run_timeout_seconds < 60 || body.run_timeout_seconds > 86400) { notice("处理时限须为 1–1440 分钟。", "submit-notice"); return; }
  const started = epoch; submitting = true; busy("submit-task", true); busy("new-task", true); notice("", "submit-notice");
  let accepted = false;
  try {
    const response = await api("/v2/runs", body, operationKey("submit:" + JSON.stringify(body)));
    if (!response.run?.run_id) throw new APIError("INVALID_RESPONSE");
    accepted = true; draftAccepted = true; el("create-dialog").close();
    rows = [response.run, ...rows.filter(run => run.run_id !== response.run.run_id)]; renderRows();
    notice("任务已创建。任务编号：" + response.run.run_id);
    await detail(response.run.run_id); await list();
  } catch (error) {
    if (error?.code !== "SESSION_CHANGED" && error?.code !== "UNAUTHORIZED") failure(error, accepted ? "notice" : "submit-notice");
    else failure(error);
  } finally { if (started === epoch) { submitting = false; busy("submit-task", false); busy("new-task", !options.length); } }
}
el("session-form").addEventListener("submit", event => {
  event.preventDefault(); const key = el("credential").value; clearSession(); credential = key; busy("connect", true);
  const started = epoch;
  guard(async () => {
    try {
      const value = await api("/v2/identity");
      if (!roles[value?.role]) throw new APIError("INVALID_RESPONSE");
      identity = value;
      text("person-role", roles[value.role]); text("avatar", roles[value.role].slice(0, 1)); text("account-role", "当前角色：" + roles[value.role]);
      el("login").hidden = true; el("session-info").hidden = false; el("workbench").hidden = false;
      el("new-task").hidden = value.role !== "operator"; busy("new-task", true);
      if (value.role === "operator") {
        const choices = await part("/v2/ui-options");
        if (started !== epoch) return;
        options = Array.isArray(choices.value?.items) ? choices.value.items : [];
        for (const [index, option] of options.entries()) { const entry = node("option", option.name); entry.value = String(index); el("processing-option").append(entry); }
        busy("new-task", !options.length);
        notice(choices.error ? "处理方式暂时无法读取，请重新连接后重试。" : !options.length ? "当前没有已启用的处理方式，可联系工作空间管理员配置。" : "", "options-note");
      }
      await list();
    } finally { if (started === epoch) busy("connect", false); }
  });
});
el("logout").addEventListener("click", () => { clearSession(); notice("已退出并清除当前内容。"); });
el("new-task").addEventListener("click", newTask);
el("submit-form").addEventListener("submit", submit);
el("query-form").addEventListener("submit", event => { event.preventDefault(); guard(() => detail(el("run-query").value.trim())); });
el("more").addEventListener("click", () => guard(() => list(true)));
el("refresh").addEventListener("click", () => guard(() => list()));
el("reload-detail").addEventListener("click", () => guard(() => selected && detail(selected.run.run_id)));
el("nav-tasks").addEventListener("click", showList); el("back").addEventListener("click", showList);
el("focus-approval").addEventListener("click", () => setFilter("awaiting")); el("focus-unknown").addEventListener("click", () => setFilter("unknown"));
el("search").addEventListener("input", event => { query = event.target.value; renderRows(); });
for (const button of document.querySelectorAll("[data-filter]")) button.addEventListener("click", () => setFilter(button.dataset.filter));
document.querySelector("[role=tablist]").addEventListener("keydown", event => {
  const tabs = [...document.querySelectorAll("[data-filter]")], index = tabs.indexOf(document.activeElement);
  const next = event.key === "ArrowRight" ? (index + 1) % tabs.length : event.key === "ArrowLeft" ? (index + tabs.length - 1) % tabs.length : event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : -1;
  if (next < 0) return;
  event.preventDefault(); tabs[next].click(); tabs[next].focus();
});
for (const button of document.querySelectorAll("[data-close]")) button.addEventListener("click", () => button.closest("dialog").close(""));
for (const dialog of document.querySelectorAll("dialog")) dialog.addEventListener("cancel", () => { dialog.returnValue = ""; });
el("session").addEventListener("click", () => el("account-dialog").showModal());
el("help").addEventListener("click", () => el("help-dialog").showModal());
el("copy-id").addEventListener("click", () => guard(async () => {
  if (selected) { await navigator.clipboard.writeText(selected.run.run_id); notice("已复制完整任务编号。"); }
}));
for (const kind of ["approve", "reject", "cancel", "reconcile"]) el(kind).addEventListener("click", () => guard(() => command(kind)));
window.addEventListener("hashchange", () => { if (location.hash === "#tasks") showList(); });
text("today", new Date().toLocaleDateString("zh-CN", { year: "numeric", month: "long", day: "numeric" }));
