# 可审阅 Agent 阶段版本

基线：`e2b9bad01d2d355d0b1116161c9a6c897a9cddc7`（PR #56），分支 `feat/reviewable-agent-release`。本记录只陈述本次工作，不覆盖历史冻结报告。

## 路线判断

最小定位：可恢复、受预算约束、可审计的售后 Agent，当前交付到有来源的待审方案。证据：`cmd/agent-control`、`cmd/agent-worker`、`internal/run`、`internal/runworker`、`python/jobforge_agent/support_agent.py` 及 ADR-0024；基线历史包含 S1 与 S2 实现，不能按旧 v0.6 入口判断仍是初始队列项目。

Go / PostgreSQL / gRPC 保留执行与事务责任；Python / RAG 保留业务与模型接缝；多租户、恢复、预算和审计属于安全正确性。旧 Jobs / Redis / 本地模型产物有现存入口和集成测试，作为兼容能力保留，不成为本轮扩展目标。审批写入、恢复成本量化及保留集仍未实现，本轮不引入新框架。

## 清理依据

| 类别 | 证据 | 处理 |
|---|---|---|
| 默认导航漂移 | README 演示 v0.6，产品索引缺 v0.16，v3 规划仍称 S2 未开始 | 当前范围集中在产品索引；README 聚焦 S2；详细 Jobs 内容移至独立指南 |
| Agent 上下文膨胀 | 根 AGENTS 默认要求读最早 PRD，叠加所有历史验证段落 | 短索引按任务披露；验证专题保留安全与分层边界 |
| 机械检查重复 | CONTRIBUTING、development 与 AGENTS 多份相似清单 | 人类命令集中到 verification；其余链接指向权威入口 |
| 原有 Jobs 测试 | cmd/jobforge、SDK Client、Compose 和 CI 仍消费对应实现 | 不删除；保留恢复、隔离、幂等、取消、事件保障 |
| S0 探针测试 | CI executor-process-probe 与 Python guardrails 仍显式调用 | 不是死测试；保留独立协议/生命周期边界，不冒充正式运行时验收 |
| 固定 S1 测试 | 生产 registry 仍保留 support-fixed-v1，S2 复用其方案与审计契约 | 保留；动态测试不能替代固定 profile 回归 |
| 旧规划和 PRD | 含已接受边界、取代关系和历史验收依据 | 保留 Git 与文档溯源，但移出默认阅读路径 |

没有证据支持批量删除测试。本轮先消除文档重复并修复实际缺口；不通过删测试获得通过。

## 本次验证

进行中；最终结果在阶段验收后更新。历史 S2 37/40 不算本次重跑。

## API 预算

本次授权最多 50 CNY。当前未发起付费调用：已知花费 0，预留 0，未知费用 0，可用授权 50 CNY。历史账本不作本次消费，也不作为可退款余额。
