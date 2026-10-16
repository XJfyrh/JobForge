export const terminal = new Set(["succeeded", "failed", "cancelled"]);
export const stateNames = { __proto__: null, ready: "等待处理", running: "处理中", retry_wait: "等待继续", awaiting_approval: "待审批", stopping: "正在停止", succeeded: "处理完成", failed: "处理结束", cancelled: "已取消" };
export const names = { __proto__: null, on_time: "按期交付", delayed: "交付延迟", disputed: "交付争议", insufficient: "信息不足", conflicting: "事实冲突", record_conclusion: "保存处理结论", request_information: "补充信息后继续", escalate: "需人工跟进", open: "待处理", informational_only: "仅供参考", awaiting_information: "等待补充信息", escalated: "需人工跟进", "ticket.order_id": "工单关联订单", "order.delivery_id": "订单关联物流", "delivery.usable_tracking_events": "可用物流事件", "delivery.delivered_event": "签收事件", "ticket.problem_description": "问题描述" };
const outcomes = { __proto__: null, none: "尚未生成处理方案", proposal: "方案待审阅", approved: "方案已批准，等待确认更新结果", applied: "结论已保存，工单已更新", rejected: "方案已拒绝，工单未按此方案更新", no_action: "无需更新工单", unknown: "更新结果待确认" };
export const errorMessages = { __proto__: null, UNAUTHORIZED: "凭据失效，请重新连接工作空间。", FORBIDDEN: "当前身份没有此操作权限。", NOT_FOUND: "未找到当前工作空间中可访问的任务。", INVALID_ARGUMENT: "请检查输入内容后重试。", INVALID_RESPONSE: "返回内容不完整，请刷新查询。", APPROVAL_CONFLICT: "当前方案或审批决定已变化，请刷新后重新核对。", APPROVAL_EXPIRED: "方案审批已到期，请刷新查询当前结果。", ACTION_CONFLICT: "工单内容已变化，当前方案无法更新工单。", PROFILE_UNAVAILABLE: "处理方式暂不可用，请联系工作空间管理员。", BUDGET_EXHAUSTED: "本次处理额度已用完。", RESULT_EXPIRED: "方案内容已过保留期，更新结果与费用仍可查询。", REQUEST_EXPIRED: "原业务请求已过保留期，不能再次执行。", IDEMPOTENCY_CONFLICT: "此业务请求编号已用于不同内容，请核对原请求。", DEPENDENCY_UNAVAILABLE: "请求结果尚未确认，请刷新查询；提交任务时保留原业务请求编号重试。" };
export const errorText = (code) => errorMessages[code] ?? "操作暂未完成，请刷新查询或联系工作空间管理员。";
export const display = (value, fallback = "暂未记录") => names[value] ?? fallback;
export const date = (value) => value && Number.isFinite(new Date(value).getTime()) ? new Date(value).toLocaleString("zh-CN", { hour12: false }) : "暂未记录";
export const money = (value) => Number.isSafeInteger(value) && value >= 0 ? `¥ ${(value / 1000000).toFixed(6)}` : "待确认";
export const tone = (run) => run.state === "awaiting_approval" ? "wait" : run.state === "succeeded" && run.outcome !== "rejected" ? "done" : terminal.has(run.state) ? "closed" : "progress";
export function processingResult(run, effect) {
  if (effect?.state === "applied" && effect.receipt?.ticket_id === run.ticket_id) return outcomes.applied;
  if (effect?.state === "unknown" || effect?.state === "applied") return outcomes.unknown;
  if (run.outcome === "unknown") return outcomes.unknown;
  if (run.state === "failed") return run.proposal_ref ? "方案已保存，处理未完成" : "处理未完成";
  if (run.state === "cancelled") return run.proposal_ref ? "后续处理已取消，方案已保存" : "后续处理已取消";
  return outcomes[run.outcome] ?? (run.state === "awaiting_approval" ? "方案待审阅" : stateNames[run.state] ?? "处理状态待确认");
}
export function matches(run, filter, effect) {
  if (filter === "awaiting") return run.state === "awaiting_approval";
  if (filter === "ended") return terminal.has(run.state);
  if (filter === "processing") return ["ready", "running", "retry_wait", "stopping"].includes(run.state);
  if (filter === "unknown") return processingResult(run, effect) === outcomes.unknown;
  return true;
}
export function effectInfo(run, effect, error) {
  const receipt = effect?.receipt;
  if (!error && effect?.state === "applied" && receipt?.ticket_id === run.ticket_id) return { applied: true, title: "工单已更新", description: `处理结论已保存，工单已标记为「${display(receipt.ticket_status, "已记录的状态")}」。`, tone: "done" };
  if (error || effect?.state === "unknown" || effect?.state === "applied" || ["unknown", "applied"].includes(run.outcome)) return { title: "更新结果待确认", description: error ? errorText(error) : "当前尚未确认完整的工单更新记录，请核对已有结果。", tone: "pending" };
  if (run.outcome === "rejected") return { title: "方案已拒绝", description: "工单未按此方案更新。", tone: "neutral" };
  if (run.state === "cancelled") return { title: "已取消后续处理", description: "后续步骤已停止，已发生的工单更新不会被撤回。", tone: "neutral" };
  return { title: "", description: "", tone: "neutral" };
}
const claimText = { __proto__: null,
  delivered_not_late: "签收时间未超过约定交付时间。", delivered_late: "签收时间已超过约定交付时间。", outstanding_not_overdue: "尚未签收，仍在约定交付时间内。", outstanding_overdue_lt48: "尚未签收，已超时但不足 48 小时。", outstanding_overdue_ge48: "尚未签收，已超时至少 48 小时。",
  non_receipt: "已有签收记录，客户仍反馈未收到货物。", wrong_address: "客户对交付地址提出异议。", unauthorized_recipient: "客户对签收人提出异议。", unauthorized_safe_place: "客户对货物放置位置提出异议。", lost: "物流记录显示货物丢失。", damaged: "物流记录显示货物损坏。", returned_to_sender: "物流记录显示货物退回发件人。",
  pre_handover: "交接前后的物流记录存在冲突。", same_time: "同一时间的物流记录存在冲突。", source_key: "来源记录之间存在冲突。", post_delivery: "签收后的物流记录存在冲突。", order_delivery: "订单与物流关联信息不一致。", informational_no_action: "工单仅作信息查询，无需变更状态。", preserve_escalated: "工单已需人工跟进，保留当前标记。"
};
export function claimDescription(claim) {
  if (claim.kind === "missing") return `需要补充${display(claim.field, "必要信息")}。`;
  if (claim.kind === "correction") return "后续物流事件修正了先前记录。";
  return claimText[claim.test ?? claim.type ?? claim.status ?? claim.mode] ?? "该项依据需结合原始业务记录核对。";
}
export function sourceName(ref) {
  if (ref.startsWith("business-policy:")) return "处理政策";
  return { __proto__: null, ticket: "工单记录", order: "订单记录", delivery: "物流记录" }[ref.split(":").at(-1)] ?? "业务记录";
}
// Resolve only the stored evidence and pointer; never substitute a nearby fact.
export function sourceValue(steps, ref, pointer) {
  if (typeof pointer !== "string" || !/^\/(ticket\/|order\/|delivery\/)?(description|subject|status|order_id|delivery_id|ticket_id|promised_delivery_at|ordered_at|observed_at|text|source|missing|missing_reason|events\/\d+\/(event_id|occurred_at|status|note))$/.test(pointer)) return undefined;
  for (const step of steps) {
    const output = step.output;
    if (!output?.evidence_refs?.includes(ref)) continue;
    const queue = [output.content]; let visited = 0;
    while (queue.length && visited++ < 2000) {
      const value = queue.shift();
      if (!value || typeof value !== "object") continue;
      if (value.evidence_ref === ref) {
        const result = atPointer(value, pointer);
        if (result !== undefined) return result;
      }
      queue.push(...Object.values(value).filter(item => item && typeof item === "object"));
    }
    if (output.evidence_refs.length === 1) {
      const result = atPointer(output.content, pointer);
      if (result !== undefined) return result;
    }
  }
  return undefined;
}
function atPointer(value, pointer) {
  for (const segment of pointer.slice(1).split("/")) {
    const key = segment.replaceAll("~1", "/").replaceAll("~0", "~");
    if (!value || typeof value !== "object" || !Object.hasOwn(value, key)) return undefined;
    value = value[key];
  }
  return ["string", "number", "boolean"].includes(typeof value) ? value : undefined;
}
