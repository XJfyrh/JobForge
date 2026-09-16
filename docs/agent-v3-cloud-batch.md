# S1 固定云端批次运行指南

本页对应 [PRD v0.13](product/JobForge_PRD_v0.13.md) 与 [ADR-0021](adr/0021-first-cloud-batch-admission-and-launcher.md)。入口使用真实 DeepSeek `deepseek-flash` 完成主线方案推理，本地 Ollama `all-minilm:22m` 只做 embedding。固定策略为 `support-fixed-v1`，版本为 `linux-v2-audit-runtime-1`，关闭 thinking，输出上限 1024 tokens。

两个租户各 20 案共享一个有界 batch，期限 6 小时，Worker、tenant、profile 容量均为 1。每案最多一次 Submit 尝试；方案停在 `awaiting_approval` 供评估，不批准方案、不写工单。[首批实际结果](evidence/agent-v3-s1-first-cloud-2026-09-16.md)未通过：15案执行结束、第16案中断，余下24案未尝试；本页命令不能用于重启已经停止的原批次。

2026-09-17 收尾授权由 [PR #53 的 PRD v0.14 / ADR-0022](https://github.com/XJfyrh/JobForge/pull/53) 已独立审查并合并，授权现已生效：本次全部新增批次累计不超过 **5 CNY**。启动前必须确认前批派发者实际退出，按当前准入合同核对原调用及上界，并从 5,000,000 microyuan 扣除本授权已启动批次的 known 与未释放 monetary hold；共享 batch 只计一次，不叠加 tenant/family 镜像。原首批费用单列，原 unknown/hold 和所有记录保留。新授权不恢复旧批，也不允许跨批挑选成功案例拼成 40 案。生成器支持 `--batch-cost-microyuan`（整数 1～5,000,000），省略时为 5 CNY。后续批次必须显式传入不超过剩余额度的值；仅共享 batch 额度降低，tenant/family/profile 不变，不手改已冻结配置。

本页的[独立干净环境复现](evidence/agent-v3-s1-reproduction-2026-09-17.md)已完成必要部署/SDK/停止检查，未调用收费模型，不代表新的 40 案已经验收。

**当前停止状态：** [本次新批真实报告](evidence/agent-v3-s1-closeout-2026-09-17.md)为14方案完成、DEV-015中断、25未尝试；其chat完整响应/计量不可得，`CHAT_USAGE_UNKNOWN`冻结并保留全额hold。后续 [ADR-0023](adr/0023-held-unknown-cross-batch-admission.md) 随 PR #54 合并后，允许符合条件的已停止传输失败保留全部 hold，按剩余额度准入独立新批；不要求补造旧 known/普通观察/Commit。身份失配、计量超界、报告冲突等不适用。以下命令仍须绑定最新账本、具体授权与构建，不能重启原批。

## 环境、数据库与安装

需要 PowerShell 7、Docker Desktop Linux 容器、Go 与 Python；版本及安装见[开发指南](development.md)。从仓库根创建独立评估环境：

```powershell
$repo = (Get-Location).Path
$batch = 'E:\JobForge-private\support-cloud-new'
python -m venv "$batch/venv"
$py = "$batch/venv/Scripts/python.exe"
& $py -m pip install ./sdk/python ./python
```

`$batch`、身份和期限由本次受审部署记录一次确定；先创建其父目录。秘密、原始响应、配置和证据均放仓库外。默认 `deploy/compose.agent.yaml` 的 project 为 `jobforge-agent-v3`，业务/控制 PG 端口为 5434/5435，不能把它当作可随意清空的测试环境。两种部署路线必须区分：

| 路线 | 业务数据库 | 控制数据库与清理 |
|---|---|---|
| 保留现有真实数据开展已授权新批 | `jobforge_support_v2_20260916`，继续使用原 `support-v2-compose.override.yaml`，不重新 seed/index | 同一 control-postgres 实例中的新库 `jobforge_s1_closeout_20260917`；旧 `jobforge_agent` 全部保留；不执行整个 project 的 `down --volumes` |
| 从干净环境复现 | 独立 project/卷中的 `jobforge_business`，按下节准备 v2 seed/index | 独立 project/卷中的控制库；先导出停止事实，只清理该 project |

旧控制库的 tenant budget `(scope, scope_key)` 唯一；不能通过给 `tenant-north/south` 换账户 UUID 在同库覆盖原账户。2026-09-17 首次新准备的 bootstrap 因此被拒绝，未 Submit、未收费、未创建 attempted；旧库保留已登记的 disabled profile 和零用量新 batch。后续把 **control 和 support-cloud 的 DSN 同时**指向独立新控制库，沿用该次已冻结的新身份/配置，保留失败日志，未恢复旧收费批次。跨库累计仍以原始账本及部署记录核对，不因新库而重新获得费用授权。

保留部署的外部覆盖文件形状如下；这里的数据库口令是仓库已有合成开发身份。业务 override 与控制 override 可以分文件，必须都纳入后续 `$dc`：

```yaml
services:
  business:
    environment:
      JOBFORGE_BUSINESS_DSN: postgres://jobforge_business_reader:jobforge_business_reader@business-postgres:5432/jobforge_support_v2_20260916?sslmode=disable
  control:
    environment:
      JOBFORGE_AGENT_DSN: postgres://jobforge_agent:jobforge_agent@control-postgres:5432/jobforge_s1_closeout_20260917?sslmode=disable
  support-cloud:
    environment:
      JOBFORGE_AGENT_DSN: postgres://jobforge_agent:jobforge_agent@control-postgres:5432/jobforge_s1_closeout_20260917?sslmode=disable
```

新库由管理员在 bootstrap 前明确创建一次，例如 `docker exec jobforge-agent-v3-control-postgres-1 createdb -U jobforge_agent jobforge_s1_closeout_20260917`；库已存在时停止核对，不 drop/recreate，不对旧库作 reset。创建新空库不等于收费授权；仍需 disabled bootstrap、实际 inspect 和完整启动前置。

干净复现则使用明确的独立 project、新 volumes 和不同宿主端口；仅改 project 名不能避免固定端口冲突。下面只启动无收费 profile 的准备环境，`!override` 用于**替换**端口而非追加端口（本次 Compose 5.3.1 支持）：

```powershell
$reproProject = 'jobforge-s1-repro-new'
$reproOverride = "$batch/repro.ports.yaml"
@'
services:
  business-postgres:
    ports: !override ["127.0.0.1:55461:5432"]
  control-postgres:
    ports: !override ["127.0.0.1:55462:5432"]
  business:
    ports: !override ["127.0.0.1:18094:8092"]
  control:
    ports: !override ["127.0.0.1:18095:8093", "127.0.0.1:19095:9093"]
  ollama:
    ports: !override ["127.0.0.1:11437:11434"]
'@ | Set-Content -LiteralPath $reproOverride -Encoding utf8
$reproDC = @('-p', $reproProject, '-f', 'deploy/compose.agent.yaml',
             '-f', $reproOverride, '--profile', 'control', '--profile', 'models')
docker compose @reproDC config --services
docker compose @reproDC up -d --wait business-postgres control-postgres ollama
```

接着按业务指南执行时统一使用 `@reproDC`：宿主 business DSN 改为端口 55461、Ollama origin 改为 `http://127.0.0.1:11437`，HTTP 为 18094；控制 bootstrap/serve 使用本 project 的 `jobforge_agent` 库与空配置，SDK 查询端口 18095。构建上下文和配置挂载仍来自本仓库。不要输出含秘密的完整 Compose config。后续如明确授权在该新环境收费，再把相同 `-p`/端口 override 加入下文 `$dc`，业务/控制 DSN 使用本环境实际库名。已执行的工件重放路线没有启动 Ollama，其精确结果见[复现记录](evidence/agent-v3-s1-reproduction-2026-09-17.md)。

## 准备的文件

业务侧先按[业务指南的干净启动命令](agent-v3-business.md#从干净环境启动windows-powershell)依次完成 PostgreSQL `migrate --initialize`、loader 身份导入 `examples/support-agent/runtime/seed.json`、固定 MiniLM 的真实 `prepare`、两个租户的 `publish-index`、reader 身份启动 HTTP 和 readiness。独立环境按上一节替换 Compose 参数、DSN 与宿主端口。需要部署的是已受审的 `support-dev-2026-09-16-v2` 数据和 `delivery-policy-dev-v2` 政策；固定模型 digest/384 维及语料 hash 必须匹配。

也可重放已有真实 embedding 工件：先验证其原 prepare receipt 和 upload SHA256，再分别 `publish-index --tenant tenant-north/south --file <vectors.json>`。这只证明工件/索引部署可复现，不是新 embedding 或检索质量验收。干净库发布会生成新的 index UUID，必须把实际返回的 UUID、content hash、IndexProfile 和 hash 写入新 source；不能照抄旧环境 UUID。保留部署则沿用实际 v2 索引，收费期间不重新 seed 或重建索引。容器网络地址固定为 `http://business:8092`、`http://ollama:11434`，更换宿主端口不修改 profile 内 origin。

把 [source 样例](../deploy/support-cloud.source.example.json)复制到仓库外，保留受审 definition，并填入四个本地 `{path, sha256}` 引用。相对路径以 source 文件所在目录为基准：

| 字段 | 实际文件 |
|---|---|
| `build_receipt` | 本次受审源码、构建镜像和安装包的部署记录；镜像在生成 profile 前构建，profile/manifest 由只读挂载提供 |
| `data_review` | 固定开发数据的审查记录，按事实标注 Agent 或人工审查 |
| `scoring_review` | 完整评分器、评分规则及反例的审查记录 |
| `price_snapshot` | 当天核对的官方定价快照；摘要同时等于 `definition.price.source_sha256` |

样例的 receipt 路径和部分摘要故意留空，不能直接运行。`prepare-support` 只读这些本地文件与 `--repo` 指定的源码，不读取 DSN、凭据或网络。schema、runtime manifest、seed 均核对原始文件 SHA256；prompt 摘要取完整 `support_adapter.py`，adapter 摘要取排序后的 `python/jobforge_agent/*.py` 路径/文件摘要，复用 `Fingerprint("jobforge.support.adapter-source.v1", ...)` 的长度前缀编码。源码变更后使用重新核对的 source，不绕过不匹配检查。

秘密另放外部目录的两个文件：`worker.json` 为 `{"control_token":"…","tenants":{"tenant-north":{"business_read_key":"…","deepseek_api_key":"…"},"tenant-south":{"business_read_key":"…","deepseek_api_key":"…"}}}`；`driver.json` 为 `{"tenant-north":"…","tenant-south":"…"}`，值是相应租户的控制面 operator key。Worker 只持业务 reader key；控制面的可信 Capture 使用独立 operator key。`JOBFORGE_CLOUD_WORKER_KEYS` 是专属 Worker ID 到同一个 `control_token` 的 JSON 映射。既有 Compose 中的 public/business key 和数据库口令均为本地开发身份，文件内容须与对应服务配置匹配；DeepSeek key 只进入外部 `worker.json`。

构建镜像运行 `docker build -f deploy/Dockerfile.agent-worker -t jobforge-agent-worker:cloud .`，再运行 `docker build -f deploy/Dockerfile.support-cloud -t jobforge-support-cloud:s1 .`。后者只加入 launcher、driver、export 和安装后的 SDK，不包含评分器、gold 或合成 registry。记录实际源码 commit、两镜像及安装包摘要，核对镜像中的 prompt/schema/registry。构建和离线 prepare 不会调用模型；在正式启用前另用官方资料和账号只读接口核对当天模型/价格/余额，不添加收费探针。

## 三分钟操作路线

以下从仓库根目录使用 PowerShell 7。三分钟指阅读操作路线，构建和真实推理耗时不作承诺。`$profileID`、`$workerID`、`$batchID`、`$batchKey`、`$northID`、`$southID`、`$validFrom` 使用本批已记录的明确值；三个账户 ID 为不同 UUID，起点为 UTC RFC3339。`$batchCostMicroyuan` 是启动前按各已启动共享 batch 的 known/held 核对并保存的剩余额度，必须显式填写；本次上一取样为 2,846,003，不把旧取样当作永久许可。不要在重跑时生成新 ID 或新期限。

先准备外部 source、秘密、构建记录及目录；`prepared` 必须尚不存在。固定 state 目录属于这次部署，不能通过更换路径重启。Linux 主机上让容器 UID/GID 65532 可写 state、exports、outbound，可读秘密文件，限制其他用户读取。

```powershell
$env:JOBFORGE_CLOUD_CONFIG_DIR = "$batch/prepared"
$env:JOBFORGE_CLOUD_SECRETS_DIR = "$batch/secrets"
$env:JOBFORGE_CLOUD_STATE_DIR = "$batch/state"
$env:JOBFORGE_CLOUD_EXPORT_DIR = "$batch/exports"
$env:JOBFORGE_CLOUD_OUTBOUND_DIR = "$batch/outbound"
# 从外部 worker.json 安全读取，不打印映射或凭据。
$workerSecret = Get-Content -LiteralPath "$batch/secrets/worker.json" -Raw | ConvertFrom-Json
$env:JOBFORGE_CLOUD_WORKER_KEYS = @{$workerID=$workerSecret.control_token} | ConvertTo-Json -Compress
$workerSecret = $null
$businessDB = 'jobforge_support_v2_20260916' # 干净独立环境使用其实际库名
$businessOverride = 'E:\JobForge-private\support-v2-compose.override.yaml'
$controlOverride = "$batch/control.override.yaml"
$dc = @('-f', 'deploy/compose.agent.yaml', '-f', 'deploy/compose.support-cloud.yaml',
        '-f', $businessOverride, '-f', $controlOverride,
        '--profile', 'control', '--profile', 'models', '--profile', 'cloud')
# 干净路线另加 '-p', '<独立project>' 与端口 override；不要混用保留环境。

go run ./cmd/agent-control prepare-support --repo $repo --source "$batch/source.json" `
  --profile-id $profileID --worker-id $workerID --batch-id $batchID --batch-key $batchKey `
  --north-account-id $northID --south-account-id $southID `
  --valid-from $validFrom --batch-cost-microyuan $batchCostMicroyuan `
  --out $env:JOBFORGE_CLOUD_CONFIG_DIR
& $py -m tools.support_evaluation.assemble register `
  --config $env:JOBFORGE_CLOUD_CONFIG_DIR --out "$batch/registration.json"
```

每段成功后再执行下一段；任何配置或检查失败都保留原产物并停止启动。生成的六个文件是 `control.disabled.json`、`control.enabled.json`、`worker.json`、`executor.json`、`launch.json`、`rows.jsonl`。两份 control 仅启用集合不同，最后一个文件已有固定次序的全部 40 条 `unattempted`。**评分 registration 在第一个 Submit 前生成并冻结**，不得事后从结果补填。launch 绑定 profile/price、四份配置摘要与三个构建/审查 receipt 摘要。

确认目标控制库与两服务 DSN 一致后，先用 disabled 配置初始化本批账户，再只读检查；这两个命令不会启动 Worker。bootstrap 幂等沿用原金额、期限与冻结状态，不执行清零。inspection 须显示 migrations/config 匹配、账户未消费/未冻结，本批的 business request/Run/call 及专属 Worker session/startup 历史均为零；零 usage 本身不代表未启动。任何失败均先保留日志与已创建事实，不覆盖 prepared/registration，不自动换身份或新库绕过已发生的 attempted/session/Submit。

```powershell
docker compose @dc up -d --wait control-postgres
# 控制库已按上一节明确创建；业务/索引已准备完毕。
docker compose @dc up -d business ollama
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
  psql -h 127.0.0.1 -U jobforge_business_reader -d $businessDB -X -q -A -t `
  -v ON_ERROR_STOP=1 > "$batch/business-before.json"

$containerName = "jobforge-support-$batchKey"
docker compose @dc run --name $containerName --no-deps support-cloud `
  1> "$batch/launcher-stdout.log" 2> "$batch/launcher-stderr.log"
$batchExit = $LASTEXITCODE
$batchExit | Set-Content -LiteralPath "$batch/launcher-exit.txt"
# 保留具名容器，只取有界状态；不输出整个 Config.Env。
docker inspect $containerName --format '{{json .State}}' > "$batch/container-state.json"
docker inspect $containerName --format '{{.Image}} {{.RestartCount}} {{.HostConfig.Init}} {{.HostConfig.RestartPolicy.Name}}' `
  > "$batch/container-runtime.txt"

# 容器退出后，无论 batchExit 是否为零，均保留原状态并采集只读后样本。
Get-Content -Raw tools/support_evaluation/business_audit.sql | `
  docker compose @dc exec -T -e PGPASSWORD=jobforge_business_reader business-postgres `
  psql -h 127.0.0.1 -U jobforge_business_reader -d $businessDB -X -q -A -t `
  -v ON_ERROR_STOP=1 > "$batch/business-after.json"
```

容器配置为 `init: true`、`restart: "no"`。launcher 再次只读 inspect，通过后先持久记录 `attempted.json`，再启动唯一 Go Worker 和 SDK driver；任一进程异常或期限到达均停止两个子进程并 Wait。它不替代 PG 的 lease、fencing、chat guard 或预算事务。driver 先原子保存 40 行，再逐行保存 `submission_attempted` 并作唯一 Submit。提交结果不明确即停止，不换 key 重发；只有持久完成的方案/no_action，或符合 ADR-0020 的完整第二纠正失败例外，才能推进下一案例。

## 停止后的只读导出与评分

原始导出位于 `$batch/exports/<UTC目录>`，包含全部 40 行 `rows.json`、逐案 SDK 原始响应及抓取摘要、`evidence.json`、`events.json` 和完成记录。`state/<batch UUID>` 保留 setup/attempted/stopped；`outbound` 保留实际 HTTP 边界的有界元数据。不要删除状态目录、重启收费容器、重置账户或换 batch 续跑；独立后续新批只能按已接受的累计授权重新准入。

`stopped.json` 的 `stop_reason` 区分 `worker_exited`、`driver_exited`、`signal`、`batch_deadline` 和 `launcher_error`，并保存两个子进程实际 `returncode`（未创建为 null，信号退出为负数）。`outbound/<physical_call_id>.failure.json` 在响应读取不完整时追加固定 `stage`（send/headers/body）、`reason` 与 `buffered_bytes`（出错前已缓冲字节数，非完整响应长度）；可区分 header 拒绝、大小限制、HTTP错误/超时与取消，不包含原始异常或正文。文件只用于诊断，不参与许可、计量或评分完整性。Worker 的固定错误分类和清理 receipt 字段写入上述 stderr 文件；不记录原始子进程输出、RPC 错误文本或模型正文。`graceful` 只表示在宽限内完成 Wait，不代表内部执行或清理成功。首批旧记录缺少这些字段，不能据此反推当时的触发来源；见[后续最小修复](evidence/agent-v3-s1-cloud-fixes-2026-09-16.md)。

已知 Run 的晚到报告或中断导出只能追加到新目录；下面的 export 模式只做 SDK 读取，不启动 Worker、不 Submit。将 `<原UTC目录>` 替换为实际目录名，保留原始导出。接纳未知且没有 Run ID 的行仍为提交未知，不能推断为零费用。

```powershell
docker compose @dc run --rm --no-deps --entrypoint python support-cloud `
  -m tools.support_evaluation.driver export `
  --original /var/lib/jobforge/exports/<原UTC目录>

# 选择实际完整的原始或追加导出目录，输出文件必须是新文件。
& $py -m tools.support_evaluation.assemble collect --registration "$batch/registration.json" `
  --export "$batch/exports/<选定UTC目录>" --outbound "$batch/outbound" `
  --before "$batch/business-before.json" --after "$batch/business-after.json" `
  --out "$batch/evidence.json"
& $py -m tools.support_evaluation.score --registration "$batch/registration.json" `
 --evidence "$batch/evidence.json" > "$batch/report.json"
```

使用安装后的 SDK 读取实际方案/结果的最小例子（凭据只从文件读取，打印有界元数据；`<实际Run UUID>` 来自原导出）：

```python
import json
from pathlib import Path
from jobforge import RunClient

keys = json.loads(Path(r"E:\JobForge-private\support-cloud-new\secrets\driver.json").read_text())
with RunClient("http://127.0.0.1:8093", keys["tenant-north"]) as client:
    run = client.get("<实际Run UUID>")
    result = client.result(run.run_id)
    calls = client.calls(run.run_id)
    print(run.run_id, run.state, result.kind, result.ref, len(calls.items))
```

完整步骤正文通过 driver/export 保存于受控目录，不打印到终端。方案完成后的 Run 可能在原 deadline 到来后转为 failed；必须保留当时方案完成的原始导出，新状态只追加记录，不能覆盖或重新解释历史评分。

业务 SQL 使用只读、repeatable-read 事务记录事实表摘要及该角色的写权限；排除接纳允许创建的 snapshots。前后摘要相同不单独证明从未发生临时写入，须与部署只读角色、固定业务 API 及实际请求记录一起核对。`dispatch_attempt` 记录早于最后许可检查，不能单独证明已经发出 HTTP；缺失、部分记录或无响应保持证据不完整，不以完整 proposal 补证。

报告始终以 40 为分母，保留失败和未尝试；分别报告协议、来源、业务、安全，区分 observed usage、settled known 和 unknown/full hold。评分进程退出 0 只表示生成报告，不表示通过；`actual_acceptance_evidence_complete` 也只是完整性条件。真实 40 案未全部执行或安全硬失败非零时，不宣称 S1 完成。历史 [W4 Claim 基准失败与 AT-25 跳过](agent-rag-review.md)继续保留，本批结果不覆盖这些记录。

## 主动停止和隔离清理

主动停止本次具名收费容器用 `docker stop --time 10 $containerName`，随后 `docker wait $containerName` 并按上文采集 State/退出事实、业务 after、stopped receipt 和追加 export。停止不撤回已发 HTTP、不释放 unknown hold，也不允许再次 start/run 原批；退出码和 `children_reaped`、原 Worker/组消失共同核对，不能只看命令返回成功。

确认退出事实和全部证据已留存后，可用 `docker rm $containerName` 只删除该退出容器；保留全部外部 state/attempted、原始导出、旧/新真实数据库与账户。若要查询旧批，只对原库启动无 Worker/disabled profile 的读取控制面；不要把新 DSN 下的 404 当作旧 Run 消失。

只有明确建立的可重建复现 project 才执行下列清理，`$reproDC` 必须始终包含它自己的 `-p` 和覆盖文件：

```powershell
docker compose @reproDC stop --timeout 10
docker compose @reproDC ps -a --format json > "$batch/repro-stopped.jsonl"
docker volume ls --filter "label=com.docker.compose.project=$reproProject"
docker compose @reproDC down --volumes
docker ps -a --filter "label=com.docker.compose.project=$reproProject"
docker volume ls --filter "label=com.docker.compose.project=$reproProject"
```

核对清理对象仅为该独立 project；不对保留真实批次的 `jobforge-agent-v3` 执行此命令，不运行系统 prune。复现无需再次收费跑 40 案；真实推理、独立评分、最终提交的必需 CI 和独立审查分别交付，全部完成前不宣称 S1 闭合。
