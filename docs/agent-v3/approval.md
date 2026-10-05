# 人工审批与工单回执

合同见 [PRD v0.18](../product/JobForge_PRD_v0.18.md) 和 [ADR-0026](../adr/0026-approval-actions-and-receipt-recovery.md)，实施与分层验收见[当前状态](../status.md)。仅 schema 4、`approval_policy=ticket_resolution_v1` 和 `linux-v2-approval-runtime-1` 登记能力可以写入；旧 profile 的方案保持只读。

## 审阅和决定

HTTP 身份由受信 `JOBFORGE_AGENT_API_KEYS` 配置提供 `tenant_id/role/actor_id`。批准必须使用独立 `approver` 和稳定 actor；轮换 token 保持同一 actor。operator 负责提交、取消、retry、终态 reconcile，不因此获得审批权。GET 允许 reader/operator/approver 查询本租户；跨租户返回 NOT_FOUND。

`GET /v2/runs/{id}/approval` 返回原持久方案、hash、许可截止、首次审批 actor/时间和当前可用性。POST 仅接受批准/拒绝、原 hash、独立 Idempotency-Key，不接受编辑后的方案。许可为原方案提交后1小时与 Run deadline 的较早者；批准及恢复不延长。只在核验方案后执行以下例子：

```python
import os
from jobforge import RunClient

with RunClient(os.environ["JOBFORGE_API_URL"], os.environ["JOBFORGE_APPROVER_KEY"]) as client:
    view = client.approval(os.environ["JOBFORGE_RUN_ID"])
    decision = client.decide_approval(
        view.run_id, "approve", view.proposal_hash,
        idempotency_key="review-ticket-first-decision",
    )
```

相同 key、actor、决定和 hash 重放首次操作，不再次排队。同键异内容为 CONFLICT；另键改变已接受 actor/决定/hash 为 APPROVAL_CONFLICT。当前权限撤销后不能靠旧 key 重放。拒绝完成为 succeeded/rejected，无动作；批准在同一 Run 排队一个 `apply_ticket_resolution`，正常继续不增加 recovery_count。

## 记录与结果

Go Worker 只执行登记的结论记录、待补充或升级人工，Python/模型只使用原三个读工具。业务 `ticket_resolutions` 原子保存完整批准方案、工单 revision/status 和首次回执；不自动关闭工单。相同 operation/内容返回首次回执；当前依赖版本变化拒绝首次写入。交付保持 at-least-once，业务去重来自稳定 operation 和原子回执。

`result.disposition` 区分 none/proposal/approved/applied/rejected/unknown/no_action；none 表示没有已接受结果，no_action 表示模型明确提交无需动作。终态首次 result 不随后续核对改变。`effect()` 单独返回 none/unknown/applied：unknown 不能当作未写入，applied 只证明结论记录/标记提交。

原 `/calls` 和模型44次额度保持；`action_calls()` 独立返回原 Run 最多4次查询和4次写入许可，provider_metering=not_applicable。物理许可重放不允许再发送，接受后不退回；SDK 每个方法只发送一次 HTTP，无自动重试、轮询或后台工作。

## 取消、核对与 retry

取消先于授权阻止动作；已授权在途请求可能随后提交。原 Run 自动恢复先查回执，找到则完成，不重跑模型前缀。未找到只可在原授权仍有效、账户/profile/审计门禁通过时重发相同 operation/签名/参数。过期仍可查回执，不能生成新授权。

终态 operator 可调用 `reconcile(run_id)`，只发一次业务 GET 并保存效果，不改变 Run/首次结果、不写业务、不调用模型。持久租户 gate 最多1次/s、一个最长10s在途许可；429 RATE_LIMITED 不触发 SDK 自动重试。只有严格业务 NOT_FOUND 回应表示此次无回执，非业务协议的404、坏响应、超时/5xx不能据此推断未写入。

人工 `retry()` 在原7日窗口内创建唯一后继。有授权时先复用本地回执或查询一次：找到直接新 succeeded/applied；未找到新 failed/ACTION_OUTCOME_UNKNOWN；依赖错误不创建后继。它继承原业务请求/operation/快照/profile/预算，无重发、续签、重新规划。首次授权前失败才沿用原捕获新快照的 retry。已接受请求重放不受窗口到期影响。回执至少30日保留，不是 GET 截止；本阶段没有删除器。

## 部署配置

先升级控制迁移0027与业务0003；注册新的不可变 profile，冻结 action operation/origin/key_id/public_key_sha256。`prepare-support` 为 schema 4 输出的 Worker action_origin 与 profile 一致；[sources.py](../../tools/support_approval/sources.py) 生成新源模板，旧 S1–S3 模板/hash 不变，构建/数据/评分审查 receipt 仍须实际提供。

| 组件 | 新配置 |
|---|---|
| control | `JOBFORGE_AGENT_ACTION_SIGNING_KEYS`：key_id→32字节seed的无padding base64url JSON；私钥仅此处持有 |
| control | `JOBFORGE_AGENT_ACTION_RECEIPT_KEYS`：tenant→独立业务回执 reader token JSON；持久 S4 profile 即使停用仍需可查询 |
| Worker config | 各 tenant 新增 `action_origin`，固定匹配 profile |
| Worker credentials file | 各 tenant 新增独立 `action_reader_key/action_writer_key`；不传 Python |
| support-business | `JOBFORGE_BUSINESS_ACTION_DSN`：专用低权限 `jobforge_business_writer_login` |
| support-business | `JOBFORGE_BUSINESS_RECEIPT_DSN`：专用低权限 `jobforge_business_receipt_reader` |
| support-business | `JOBFORGE_BUSINESS_ACTION_KEYS`：token→tenant/action_reader或action_writer JSON，与原 reader/capture token 独立 |
| support-business | `JOBFORGE_BUSINESS_ACTION_PUBLIC_KEYS`：key_id→`{public_key,tenants}` JSON，公钥为无padding base64url |

上述业务四项要同时配置或全部省略；不扩大旧 runtime/loader/read pool 权限。启动检查池角色、public-key/profile 绑定及凭据分离。公钥可进入配置/证据，私钥、writer token、DSN 和完整敏感 payload 不进入日志、仓库或普通报告。密钥轮换保留旧公钥验证能力，不续签旧授权。

默认 Compose 仍为空收费登记，不自动启用写入或真实模型。真实执行必须使用经独立审查放行的有限 manifest。生产镜像只注册 support-fixed-v1/support-agent-v1；故障代理、合成 adapter 和 gold 只在测试/外部验收侧。存在 S4 决定/授权/回执时 down 拒绝丢弃审计，备份后优先前滚修复。免费机制与自然进程故障命令见[测试指南](../tests.md)，模型质量和机制触达分列报告。

有限真实验收的冻结、release、外部 Linux operator 与独立离线报告命令见[验收工具说明](../../tools/support_approval/README.md)。
