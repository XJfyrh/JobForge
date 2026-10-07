# Run 页面、观测与数据操作

S5 实现与分层证据见[验收矩阵](s5-acceptance.md)；合同见 [PRD v0.19](../product/JobForge_PRD_v0.19.md)、[ADR-0027](../adr/0027-s5-observability-and-data-lifecycle.md)。默认配置没有收费 profile。

## 页面与身份

打开控制 HTTP 服务的 `/ui/`，输入当前租户的 reader、operator 或 approver 凭据。页面通过 `/v2/identity` 读取服务端角色；operator 提交、取消和核对回执，approver 对原方案批准或拒绝，reader 只读。凭据仅保留在页面内存，切换身份、退出或401会清除查询内容；页面无跨源 API、cookie 或本地存储。

主页面呈现处理进度、方案、事实来源和实际工单更新结果。业务服务回执证明结论保存及工单标记；显示“需人工跟进”不代表已转交其他系统。查询不确定时显示“暂未确认更新结果”，operator 可核对既有回执。Run/attempt/profile/hash、原始 JSON 和账本在技术详情中。

## 独立观测环境

```powershell
docker compose -f deploy/compose.agent.yaml --profile obs up -d run-collector run-jaeger run-prometheus run-grafana
```

此 profile 使用 v3 专用 collector、Prometheus、Grafana 和 Jaeger。宿主入口为 Prometheus `http://localhost:9094`、Grafana `http://localhost:3004`、Jaeger `http://localhost:16687`；部署到其他网络时自行设置访问边界和凭据。控制与 Worker 的 metrics 端口只用于内部抓取，容器地址应显式配置；默认分别监听 `127.0.0.1:6063/6064`。

S5 operator 设置 `JOBFORGE_AGENT_WORKER_METRICS_ADDR=0.0.0.0:6064`，启动器将该地址传给 Go Worker；容器在观测私网使用 `agent-worker` 别名，与 Prometheus 抓取目标一致。控制服务的 `JOBFORGE_METRICS_ADDR` 独立配置为 `0.0.0.0:6063`。

控制、Worker、业务服务设置 `JOBFORGE_OTEL_EXPORTER=otlp`、`OTEL_EXPORTER_OTLP_ENDPOINT=http://run-collector:4318`；未设置时不导出。采样率可通过 `JOBFORGE_OTEL_SAMPLE_RATIO` 指定0–1。SDK 使用部署的 OpenTelemetry provider，不在 SDK 内创建隐式全局 exporter。

每个 attempt 新建有限根 span，link 到原 Submit trace，以 Run ID / attempt_no 关联审批后继续及租约恢复。人工等待时没有常驻 span。`run.python.execute` 由 Go guardian 计时，覆盖真实进程启动至清理；`run.external_http` 的 Python 调用由已核验 IPC 的 permit→observation 区间桥接，缺失 observation 保持 unknown，许可本身不证明已发送。实际业务 HTTP 继续传播 TraceContext。该实现没有新增 Python IPC 消息或 Python collector 通道。

指标标签只使用固定 route/method、状态、步骤/调用类别和错误分类。Run、租户、审批者和调用 ID 仅作 trace 属性。PG 快照失败时面板保持缺失并触发 snapshot 告警；不得把缺失数据填成零。family/tenant/batch 是重叠预算范围，不能相加当成总支出。账本 known 是按登记费率估算的用量费用，held 是暂占额度；不是供应商已结算账单。

## 终态内容清理

仅首次终态至少7日、原费用批次已到期的 Run 可清理。活动、待审批内容保留；调用/attempt/操作键/profile、签名与回执、预算账户、unknown hold 和冻结不删除。业务库的快照、结论、幂等身份和完整回执在 S5 完整保留，覆盖至少30日；本阶段没有业务内容删除入口。

```powershell
$env:JOBFORGE_AGENT_DSN = '<专用控制库DSN>'
agent-control cleanup-terminal --database <精确控制库名> --limit 100
agent-control cleanup-terminal --database <精确控制库名> --limit 100 --apply
```

第一条是 dry-run。命令核验精确库名和控制 schema，最多100个候选；逐 Run 短事务锁定并重新检查状态/时间。实际清理后 steps/result/GET及POST approval 返回 `410 RESULT_EXPIRED`，包含既有批准、拒绝的 POST 重放。旧 Submit 键返回 `410 REQUEST_EXPIRED`，不 capture 新快照、不重建 Run。GET Run、费用和独立效果继续可查。30日晚到 usage 仍按原调用身份和窗口裁定，不因内容清理退款或解冻。

## 停写双库恢复演练

先停止控制接纳/扫描器、Worker/guardian、业务 HTTP、loader 和 writer，确认执行组已退出。对两份专用库安装精确库名的连接屏障，关闭既有应用会话；屏障保持至两库 dump 完成。仅本地管理员备份连接可进入。停写状态下逐库 `pg_dump -Fc`，记录迁移、源码/构建、dump SHA-256、行数和固定字节顺序的内容摘要。包含业务内容和签名的 dump 与凭据留在仓库外。

免费机制环境可运行仓库中的专用演练工具：

```powershell
.venv/Scripts/python.exe tools/support_lifecycle/exercise.py --review-dir <绝对审查目录> --output-dir <仓库外全新目录> --review-container <已暂停审查容器> --control-container <源控制PG容器> --business-container <源业务PG容器>
```

该入口仅接受明确标记为无云调用的新建审查库和暂停记录。它先对两库安装 TCP 屏障，再终止既有会话、dump，并恢复到全新固定 pgvector 镜像容器；逐表比较完整数据。原 HBA 在退出时分别尝试还原。失败时保留备份/恢复容器和原报告，不能将部分结果记为通过。

恢复容器默认 network=none，应用角色 NOLOGIN，不启动 Worker、扫描器、模型或业务 writer。profile 的可执行状态来自部署配置，未保存在数据库定义中；恢复读取服务使用空的可执行 profile 集合。完整数据相等后仍须用安装 SDK 验证原 Submit/审批身份重放、原动作回执防重和新提交拒绝；核对 Run、调用、费用/hold、冻结及业务版本未增加。不能在源库上 restore，或自动启用旧批次。

## 三分钟演示

预先预热模型/索引并准备一个获授权的有限批次，记录准备时间。0:00–0:40 用 operator 提交并展示订单、物流和实际政策来源；0:40–1:20 在已提交步骤后停止具名 Worker，等待自然 lease 回收，再启动同 profile Worker，展示原步骤引用和新的 attempt；1:20–2:10 查看方案并切换独立 approver 批准；2:10–3:00 切回 operator 展示真实工单标记、唯一回执及审批前后有限 spans。若使用预生成结果或加速录屏，应明确标注原始耗时，保留完整运行证据。
