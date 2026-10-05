# S4 有限验收工具

仅用于经独立审查放行的外部验收，合同见[人工审批指南](../../docs/agent-v3/approval.md)。生产 Worker 不包含本目录、故障代理、gold 或业务管理凭据。

先构建正式 `deploy/Dockerfile.agent-worker`，再将其固定 digest 作为 `deploy/Dockerfile.support-approval` 的 `WORKER_IMAGE`。operator 安装正式 SDK；Go Worker/Python 单步仍执行生产登记能力。开发数据只读挂载到 operator 的 `/app/examples/support-agent`。

`sources.py` 从当前源和已核实的官方价格快照生成 schema 4 模板；`prepare-support` 只准备配置，不启用模型。完成业务导入、真实 MiniLM 索引、隔离 S4 控制/业务数据库、独立审批 actor、公钥配置、构建与评分审查后，再生成六小时窗口。使用 `plan.py --config <prepared> --replacement-worker <registered-id> --out <outside-repo>` 冻结配置、源码、数据、价格与名单；准备文件保持原字节，第二 Worker 配置单独派生。

固定名单为 DEV-001/016/011/002/003/004/005/006/007/035，只有 006/007 各有一次回执 retry；不替换失败样本。新增累计上限为规划 chat 选择的 5 元、最多 12 个新 Run，其中 10 个源 Run、2 个后继机会；不是用户口述金额。冻结历史账户和 hold 保持不动。

外部 `release.json` 必须明确批准精确 manifest/settings/build/head，独立审阅通过，当前 head 的现有 8 项 CI 全通过，并绑定正式镜像 digest、官方价格及只读账户快照。driver 不签发 release，也不从“过去检查通过”推断当前放行。执行使用 Linux `--init`、私有凭据挂载、固定网络和新的输出目录：

```sh
python -m tools.support_approval.driver --plan /private/plan.json \
  --settings /private/settings.json --release /private/release.json --out /evidence/run
```

settings 包含预登记的两个 Worker、正式配置/凭据路径、独立 operator/approver token、gateway/business origin、只读审计 DSN，以及唯一预声明的实际 loader revision 冲突 DSN。仅控制服务持有动作签名私钥；Python 单步不接收 operator 环境或凭据。敏感 settings、原始响应、签名、SQL 证据只放私有验收目录，不写普通日志/仓库。

审批前复用原 scorer 的协议、来源、冻结 predicates 和完整只读 safety；审计未知、异常、账户冻结即停止整个批次。质量失败保留，动作机制记为 unexercised。故障仅暂停真实授权 ACK 或已验证的实际业务提交回应，在活租约内记录 PID/SIGKILL/Wait；自然恢复不修改数据库状态或时钟。冲突仅通过实际 loader 角色修改已声明工单 revision。取消后的 retry 只查/复用回执，无模型和写入重发。

driver 中途失败也尽力导出当前 Run，并分别执行最终控制与业务只读审计。离线报告将评分输入逐字段对照原 pending，以控制账本核实所有实际 Run 和 known/held，以整个业务库核实批准操作集合：

```sh
python -m tools.support_approval.report --plan /private/plan.json \
  --archive /evidence/run --traces /evidence/barriers --out /evidence/report.json
```

报告分列质量、机制触达、失败/未尝试、实际 Run 和用量。unknown/held 不记成零费；known cost 是 observed usage 的 tariff estimate，不是已结算账单。审计缺失返回 unknown 数量/费用及失败诊断，不能据此验收通过。免费工具测试见[测试指南](../../docs/tests.md)；正式真实模型结果与独立验收另行归档。
