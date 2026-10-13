# ADR-0038：先读取完整政策，再独立终裁

- 日期：2026-10-13；状态：Accepted（维护者批准分离政策读取与终裁，独立设计审查接受固定整包读取方向；实现与验证另记）。
- 仅对新候选取代 [ADR-0036](0036-s5-controlled-policy-supplement.md) 的可选 thinking 补证和 [ADR-0037](0037-status-policy-read-prerequisite.md) 的状态特判读取；旧版本与停止批次不变。

## 原因与决策

[v7 已见回归](../evidence/agent-v3-s5-v7-seen-v3-2026-10-13.md)的 004 缺 P05.1，补证入口和工具余量都存在，但首次 thinking 用满输出容量仍未提交补证或 final。提示已经要求读取定义，再加同义提醒或独立模型计划仍不能避免模型错误排除必要政策。本次不推断缺定义与输出耗尽的因果关系。

新增 `read_policy_bundle`：所有案件读取其授权快照索引内 **P01.1 至 P10.2 的固定 20 段**，不依赖案例 ID、描述关键词、模型初稿、gold 或评分。正文总计 7,738 UTF-8 字节；这是当前小型政策集的完整读取，不再把此路径称为语义检索改进。将来政策集超出固定范围时需新版本，不能自动扩大。

Agent 路径为 `read_ticket → read_policy_bundle → 调查 → marker → 一次 thinking final → submit_proposal`。Go 在任何模型调用前安排读取，Python 只执行登记单步。调查仍可自主读取订单、配送及使用原搜索工具；不再拥有终裁补证分支。读取义务不决定业务行动，最终判断仍由模型完成。

Fixed 新版本同样先读取政策包，再沿固定事实读取图执行一次相同能力的 thinking 裁决；旧 schema 12 不能充当“相同工具”的新比较基线。

## 读取与来源合同

- 新增只读 `GET /business/v1/snapshots/{id}/policies/bundle`，不接受调用方指定租户、索引、版本、别名或查询；绑定和鉴权复用现有快照接口。固定按别名排序返回 `snapshot_id` 和 `matches`。
- 每段保留实际 `index_id`、`chunk_id`、`policy_version`、`source`、`text`、`evidence_ref`；直接读取没有向量距离，不伪造 distance。响应上限 32 KiB，仅适用于新工具，旧搜索保持 top-3/8 KiB。
- 全部 20 个别名必须且只能出现一次；来源、索引、版本、正文和快照均校验。缺段、重复、替换别名、跨绑定、空正文或超限均在首次模型调用前失败关闭，不降级搜索或继续裁决。
- 来源构造、模型输入、持久方案、审批及离线复算均承认实际新步骤。若后续搜索返回同别名，正文和来源必须与已读包相同，不允许覆盖。

## 版本、计量与恢复

Agent 使用 schema **16**、`support-agent-prompt-v8`、`linux-v2-adjudication-runtime-5`；Fixed 使用 schema **17**、`support-fixed-prompt-v2`、`linux-v2-fixed-adjudication-runtime-2`。新版本 final 只有一种 `{type:"final",proposal:{...}}` 输出，提示明确六字段属于内层 proposal；不复用共享提示中的裸六字段及 1024-token 外层要求，不做格式失败重试。

新工具计为一个逻辑工具、一次实际业务 HTTP、一个独立物理调用；不计 embedding 或 chat，不新增免计量通道。仍使用每 Run 8 工具、12 chat、44 物理请求及原账户、期限与逐调用许可。终裁仍为 16,384 completion / 120 秒，不增加输出容量。协议、HTTP/OpenAPI、Proto、SDK、Go/Python runtime、profile、schema、评分和冻结入口须同步；数据库只在现有列约束确需扩展时新增 migration，不改历史 migration。

读取结果沿用 Step/Commit/lease/fencing；已提交步骤恢复时不重读。confirmed-uncommitted 按原恢复合同处理，未知仍保留 hold 并停发。审批和业务写入能力不变，生产 registry 仍只有 `support-fixed-v1`、`support-agent-v1`。

## 验证与退出

免费验证覆盖新旧隔离、双策略正常路径、整包严格形状、跨租户/快照/版本拒绝、丢段/重复/空正文/超限、来源合并冲突、final-only 拒绝裸对象和补证意图、初稿隔离、恢复/失租/取消、逐调用计量与 unknown hold。运行真实 PostgreSQL、全仓 race、固定 Linux 执行器与 HTTP/SDK/Proto 检查；历史请求哈希和评分不改变。新 schema16/17 的构建与冻结额外绑定 `internal/runprotocol/v2`、`internal/runinput`、executor v2 schema 和生成的 RPC 代码；旧版本集合与校验行为保留。

免费层及独立审查通过后才另审具体收费 manifest，从新身份完整开发 40 案开始，不续跑 v7 剩余 16 案，不选择性重跑 004。原开发 36/40 正确、38/40 完整证据、硬失败零及 004/018/DEV-020 守卫不变；完整整包不等于业务质量通过。之后才可进行同能力 Fixed 比较、新冻结及未见正式集。每批 ¥4、S5 累计 ¥28 不变；当前 observed-usage 估算 ¥13.165020，held 0。本 ADR 不放行收费 manifest。
