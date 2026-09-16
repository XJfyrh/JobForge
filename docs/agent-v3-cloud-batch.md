# S1 固定云端批次运行指南

本页对应 [PRD v0.13](product/JobForge_PRD_v0.13.md) 与 [ADR-0021](adr/0021-first-cloud-batch-admission-and-launcher.md)。入口使用真实 DeepSeek `deepseek-flash` 完成主线方案推理，本地 Ollama `all-minilm:22m` 只做 embedding。固定策略为 `support-fixed-v1`，版本为 `linux-v2-audit-runtime-1`，关闭 thinking，输出上限 1024 tokens。

两个租户各 20 案共享一个 **5 CNY、6 小时**的 batch，Worker、tenant、profile 容量均为 1。每案最多一次 Submit 尝试；方案停在 `awaiting_approval` 供评估，不批准方案、不写工单。这些额度不保证 40 案全部完成。[首批实际结果](evidence/agent-v3-s1-first-cloud-2026-09-16.md)未通过：15案执行结束、第16案中断，余下24案未尝试；本页命令不能用于重启已经停止的原批次。

## 准备的文件

业务侧先按[业务指南](agent-v3-business.md)部署已受审的 `support-dev-2026-09-16-v2` 数据、`delivery-policy-dev-v2` 政策及两个已发布索引。沿用它们的实际 UUID、content hash 与 IndexProfile，收费期间不重新 seed 或重建索引。服务网络地址固定为 `http://business:8092`、`http://ollama:11434`；主机上的 Ollama 映射端口是 11436。

把 [source 样例](../deploy/support-cloud.source.example.json)复制到仓库外，保留受审 definition，并填入四个本地 `{path, sha256}` 引用。相对路径以 source 文件所在目录为基准：

| 字段 | 实际文件 |
|---|---|
| `build_receipt` | 本次受审源码、构建镜像和安装包的部署记录；镜像在生成 profile 前构建，profile/manifest 由只读挂载提供 |
| `data_review` | 固定开发数据的审查记录，按事实标注 Agent 或人工审查 |
| `scoring_review` | 完整评分器、评分规则及反例的审查记录 |
| `price_snapshot` | 当天核对的官方定价快照；摘要同时等于 `definition.price.source_sha256` |

样例的 receipt 路径和部分摘要故意留空，不能直接运行。`prepare-support` 只读这些本地文件与 `--repo` 指定的源码，不读取 DSN、凭据或网络。schema、runtime manifest、seed 均核对原始文件 SHA256；prompt 摘要取完整 `support_adapter.py`，adapter 摘要取排序后的 `python/jobforge_agent/*.py` 路径/文件摘要，复用 `Fingerprint("jobforge.support.adapter-source.v1", ...)` 的长度前缀编码。源码变更后使用重新核对的 source，不绕过不匹配检查。

秘密另放外部目录的两个文件：`worker.json` 为 `{"control_token":"…","tenants":{"tenant-north":{"business_read_key":"…","deepseek_api_key":"…"},"tenant-south":{"business_read_key":"…","deepseek_api_key":"…"}}}`；`driver.json` 为 `{"tenant-north":"…","tenant-south":"…"}`，值是相应租户的控制面 operator key。Worker 只持业务 reader key；控制面的可信 Capture 使用独立 operator key。`JOBFORGE_CLOUD_WORKER_KEYS` 是专属 Worker ID 到同一个 `control_token` 的 JSON 映射。既有 Compose 中的 public/business key 和数据库口令均为本地开发身份，文件内容须与对应服务配置匹配；DeepSeek key 只进入外部 `worker.json`。

构建镜像前可运行 `docker build -f deploy/Dockerfile.agent-worker -t jobforge-agent-worker:cloud .`，再运行 `docker build -f deploy/Dockerfile.support-cloud -t jobforge-support-cloud:s1 .`。后者只加入 launcher、driver、export 和 SDK，不包含评分器、gold 或合成 registry。依赖安装沿用[开发指南](development.md)的 Python 环境，离线注册/评分环境同时安装 `sdk/python` 与 `python`。

## 三分钟操作路线

以下从仓库根目录使用 PowerShell 7。三分钟指阅读操作路线，构建和真实推理耗时不作承诺。`$profileID`、`$workerID`、`$batchID`、`$batchKey`、`$northID`、`$southID`、`$validFrom` 使用本批已记录的明确值；三个账户 ID 为不同 UUID，起点为 UTC RFC3339。不要在重跑时生成新 ID 或新期限。

先准备外部 source、秘密、构建记录及目录；`prepared` 必须尚不存在。固定 state 目录属于这次部署，不能通过更换路径重启。Linux 主机上让容器 UID/GID 65532 可写 state、exports、outbound，可读秘密文件，限制其他用户读取。

```powershell
$repo = (Get-Location).Path
$batch = 'E:\JobForge-private\support-cloud'
$env:JOBFORGE_CLOUD_CONFIG_DIR = "$batch/prepared"
$env:JOBFORGE_CLOUD_SECRETS_DIR = "$batch/secrets"
$env:JOBFORGE_CLOUD_STATE_DIR = "$batch/state"
$env:JOBFORGE_CLOUD_EXPORT_DIR = "$batch/exports"
$env:JOBFORGE_CLOUD_OUTBOUND_DIR = "$batch/outbound"
# JOBFORGE_CLOUD_WORKER_KEYS 已由本批的外部秘密配置设置，不打印其内容。
$dc = @('-f', 'deploy/compose.agent.yaml', '-f', 'deploy/compose.support-cloud.yaml',
        '--profile', 'control', '--profile', 'models', '--profile', 'cloud')

go run ./cmd/agent-control prepare-support --repo $repo --source "$batch/source.json" `
  --profile-id $profileID --worker-id $workerID --batch-id $batchID --batch-key $batchKey `
  --north-account-id $northID --south-account-id $southID `
  --valid-from $validFrom --out $env:JOBFORGE_CLOUD_CONFIG_DIR
python -m tools.support_evaluation.assemble register `
  --config $env:JOBFORGE_CLOUD_CONFIG_DIR --out "$batch/registration.json"
```

每段成功后再执行下一段；任何配置或检查失败都保留原产物并停止启动。生成的六个文件是 `control.disabled.json`、`control.enabled.json`、`worker.json`、`executor.json`、`launch.json`、`rows.jsonl`。两份 control 仅启用集合不同，最后一个文件已有固定次序的全部 40 条 `unattempted`。**评分 registration 在第一个 Submit 前生成并冻结**，不得事后从结果补填。launch 绑定 profile/price、四份配置摘要与三个构建/审查 receipt 摘要。

先用 disabled 配置初始化原账户，再只读检查；这两个命令不会启动 Worker。bootstrap 幂等沿用原金额、期限与冻结状态，不执行清零。inspection 须显示 migrations/config 匹配、账户未消费/未冻结，原 batch 的 business request/Run/call 及专属 Worker session/startup 历史均为零；零 usage 本身不代表未启动。

```powershell
docker compose @dc up -d control-postgres business ollama
docker compose @dc build control
docker compose @dc run --rm --no-deps --entrypoint /usr/local/bin/agent-control `
  -e JOBFORGE_AGENT_CONFIG=/etc/jobforge/cloud/control.disabled.json support-cloud bootstrap
docker compose @dc run --rm --no-deps --entrypoint /usr/local/bin/agent-control `
  -e JOBFORGE_AGENT_CONFIG=/etc/jobforge/cloud/control.disabled.json support-cloud inspect-support `
  > "$batch/setup-before.json"
docker compose @dc run --rm --no-deps support-cloud prepare-state
```

`prepare-state` 只创建本批 `prepared.json` 和排他锁；已有目录不覆盖。下一段启动挂载 enabled 配置的控制面，取得业务只读基线，然后只运行一次收费容器。以下 psql 口令是既有本地 Compose 的 reader 开发身份；换用部署凭据时保持同一只读角色。

```powershell
docker compose @dc up -d control
Get-Content -Raw tools/support_evaluation/business_audit.sql | `
  docker compose @dc exec -T -e PGPASSWORD=jobforge_business_reader business-postgres `
  psql -h 127.0.0.1 -U jobforge_business_reader -d jobforge_business -X -q -A -t `
  -v ON_ERROR_STOP=1 > "$batch/business-before.json"

docker compose @dc run --rm --no-deps support-cloud
$batchExit = $LASTEXITCODE

# 容器退出后，无论 batchExit 是否为零，均保留原状态并采集只读后样本。
Get-Content -Raw tools/support_evaluation/business_audit.sql | `
  docker compose @dc exec -T -e PGPASSWORD=jobforge_business_reader business-postgres `
  psql -h 127.0.0.1 -U jobforge_business_reader -d jobforge_business -X -q -A -t `
  -v ON_ERROR_STOP=1 > "$batch/business-after.json"
```

容器配置为 `init: true`、`restart: "no"`。launcher 再次只读 inspect，通过后先持久记录 `attempted.json`，再启动唯一 Go Worker 和 SDK driver；任一进程异常或期限到达均停止两个子进程并 Wait。它不替代 PG 的 lease、fencing、chat guard 或预算事务。driver 先原子保存 40 行，再逐行保存 `submission_attempted` 并作唯一 Submit。提交结果不明确即停止，不换 key 重发；只有持久完成的方案/no_action，或符合 ADR-0020 的完整第二纠正失败例外，才能推进下一案例。

## 停止后的只读导出与评分

原始导出位于 `$batch/exports/<UTC目录>`，包含全部 40 行 `rows.json`、逐案 SDK 原始响应及抓取摘要、`evidence.json`、`events.json` 和完成记录。`state/<batch UUID>` 保留 setup/attempted/stopped；`outbound` 保留实际 HTTP 边界的有界元数据。不要删除状态目录、重启收费容器、重置账户或换 batch 续跑。

已知 Run 的晚到报告或中断导出只能追加到新目录；下面的 export 模式只做 SDK 读取，不启动 Worker、不 Submit。将 `<原UTC目录>` 替换为实际目录名，保留原始导出。接纳未知且没有 Run ID 的行仍为提交未知，不能推断为零费用。

```powershell
docker compose @dc run --rm --no-deps --entrypoint python support-cloud `
  -m tools.support_evaluation.driver export `
  --original /var/lib/jobforge/exports/<原UTC目录>

# 选择实际完整的原始或追加导出目录，输出文件必须是新文件。
python -m tools.support_evaluation.assemble collect --registration "$batch/registration.json" `
  --export "$batch/exports/<选定UTC目录>" --outbound "$batch/outbound" `
  --before "$batch/business-before.json" --after "$batch/business-after.json" `
  --out "$batch/evidence.json"
python -m tools.support_evaluation.score --registration "$batch/registration.json" `
  --evidence "$batch/evidence.json" > "$batch/report.json"
```

业务 SQL 使用只读、repeatable-read 事务记录事实表摘要及该角色的写权限；排除接纳允许创建的 snapshots。前后摘要相同不单独证明从未发生临时写入，须与部署只读角色、固定业务 API 及实际请求记录一起核对。`dispatch_attempt` 记录早于最后许可检查，不能单独证明已经发出 HTTP；缺失、部分记录或无响应保持证据不完整，不以完整 proposal 补证。

报告始终以 40 为分母，保留失败和未尝试；分别报告协议、来源、业务、安全，区分 observed usage、settled known 和 unknown/full hold。评分进程退出 0 只表示生成报告，不表示通过；`actual_acceptance_evidence_complete` 也只是完整性条件。真实 40 案未全部执行或安全硬失败非零时，不宣称 S1 完成。历史 [W4 Claim 基准失败与 AT-25 跳过](agent-rag-review.md)继续保留，本批结果不覆盖这些记录。
