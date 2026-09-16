# Agent v3 S1-A：业务快照与真实政策检索

本切片提供可独立启动的业务依赖：40个固定英文合成工单、Go HTTP、不可变PostgreSQL快照、真实MiniLM embedding和pgvector检索。它不领取Run、不调用付费chat、不执行写入建议。DeepSeek、Run账本和固定流程仍属于S1-B/C；完整Agent执行退出条件保持不变。

## 从干净环境启动（Windows PowerShell）

需要Docker Desktop Linux容器、Go 1.26和Python 3.12。命令从仓库根目录执行；所有示例密码/业务key只用于该隔离开发项目。

```powershell
python -m venv .venv
.venv\Scripts\python -m pip install -r tools/requirements-lint.txt
.venv\Scripts\python -m pip install --no-deps ./sdk/python
$env:PYTHONPATH='python'
$out=Join-Path $env:TEMP 'jobforge-s1-acceptance'

docker compose -f deploy/compose.agent.yaml --profile models up -d --wait business-postgres ollama
docker compose -f deploy/compose.agent.yaml exec -T ollama ollama pull all-minilm:22m

$env:JOBFORGE_BUSINESS_DSN='postgres://jobforge_business_bootstrap:jobforge_business_bootstrap@127.0.0.1:5434/jobforge_business?sslmode=disable'
go run ./cmd/support-business migrate --initialize
$env:JOBFORGE_BUSINESS_DSN='postgres://jobforge_business_loader_login:jobforge_business_loader_login@127.0.0.1:5434/jobforge_business?sslmode=disable'
go run ./cmd/support-business seed --file examples/support-agent/runtime/seed.json

$prep=(.venv\Scripts\python -m jobforge_agent prepare --output-dir $out --ollama-origin http://127.0.0.1:11436 | ConvertFrom-Json).report
$prepared=Get-Content -LiteralPath $prep -Raw | ConvertFrom-Json
$vectors=Join-Path (Split-Path $prep) $prepared.upload_filename
go run ./cmd/support-business publish-index --tenant tenant-north --file $vectors
go run ./cmd/support-business publish-index --tenant tenant-south --file $vectors
docker compose -f deploy/compose.agent.yaml up -d --build business
Invoke-RestMethod http://127.0.0.1:8092/health/ready
```

每步必须成功再继续。CLI失败返回非零并脱敏错误。准备报告先写`unknown`；只有全部20段返回384维向量后才写`prepared`。该状态不等于数据库已发布；只有Go装载命令返回的`index_id`是权威引用。装载命令同时核对嵌入二进制的登记语料、原始hash及逐段文本，不接受仅声称hash正确的任意文本。

Python使用源码包`python/jobforge_agent`，本切片的三个工具不属于旧`jobforge.JobForge`客户端；后续`/v2/runs` SDK另行实现。Linux使用`export PYTHONPATH=python`、`.venv/bin/python`及相同模块/Go/Compose命令；环境变量用`export NAME=value`设置。Go可以替换为预先`go build -o support-business ./cmd/support-business`后的二进制调用。

## 三分钟演示（依赖已准备）

```powershell
$headers=@{Authorization='Bearer dev-north-operator-key'}
$body=@{schema_version=1;ticket_id='ticket-01';request_key='demo-v1'} | ConvertTo-Json -Compress
$snapshot=Invoke-RestMethod http://127.0.0.1:8092/business/v1/snapshots -Method Post -ContentType application/json -Headers $headers -Body $body
$snapshot.snapshot_id
$read=@{Authorization='Bearer dev-north-reader-key'}
Invoke-RestMethod "http://127.0.0.1:8092/business/v1/snapshots/$($snapshot.snapshot_id)/order" -Headers $read
Invoke-RestMethod "http://127.0.0.1:8092/business/v1/snapshots/$($snapshot.snapshot_id)/delivery" -Headers $read

$env:JOBFORGE_BUSINESS_READ_KEY='dev-north-reader-key'
$retrieval=(.venv\Scripts\python -m jobforge_agent retrieval --output-dir $out --ollama-origin http://127.0.0.1:11436 --business-origin http://127.0.0.1:8092 --snapshot-id $snapshot.snapshot_id | ConvertFrom-Json).report
.venv\Scripts\python tools/evaluate_policy_retrieval.py --report $retrieval --output (Join-Path $out 'scores.json')
```

再次提交相同body得到相同snapshot；相同request_key改ticket返回409。将读取key换成`dev-south-reader-key`访问北租户snapshot得到404。引用不是访问凭据；`business-evidence:<snapshot>:order`和`business-policy:<index>:<chunk>`都必须在授权快照路径内查询。政策查询接口接受固定profile和384维向量，Python工具仅向模型开放有界query文字。

检索报告保留全部20条及原始top-3，评分器独立读取evaluation标签，失败/未尝试仍计入20条分母。评测按政策ID计算Hit@3/MRR@3，而非宣称每个具体段落都准确。当前v2的真实运行见[新证据](evidence/agent-v3-s1-support-2026-09-16.md)，原v1的[验收证据](evidence/agent-v3-s1-business-2026-09-16.md)保留。准备及检索的requests计数是发送前持久化的发送意图；取消、超期或进程退出可能发生在实际发送前，不能据此推断未知调用未发生。

## 契约与边界

完整接口表和版本规则见[ADR-0016](adr/0016-business-snapshots-and-policy-retrieval.md)。HTTP创建≤4KiB、搜索≤32KiB、响应≤8KiB、并发8、每请求10秒；未知字段、大小写别名、重复字段、尾随JSON、数值null和非法向量均拒绝。read服务启动检查低权限身份及每个配置tenant的种子/完整索引；缺资源时停止启动，运行中丢失依赖时ready返回503。

固定业务观察时点保存在ticket.observed_at并冻结为snapshot.as_of；created_at另记事务捕获时间。快照事务使用repeatable-read，源表变更不影响已捕获订单、物流和旧政策；新事实需新请求键。三种工具只接受当前snapshot/order绑定，输入不能成为URL、SQL、shell或动态工具名入口。

独立Compose项目`jobforge-agent-v3`占用5434、8092、11436，业务库与旧队列5433分离。NOLOGIN owner持有schema；HTTP用reader，种子/索引用loader，迁移用bootstrap。固定登录密码仅限本地开发实例。运行时虽在tenant间共用数据库角色，但HTTP/service每条查询均绑定鉴权tenant；不宣称数据库RLS隔离。全局角色标记属于该专属实例，业务库用途标记仍逐库核验；迁移拒绝未标记数据库和核心jobs表。

本切片使用旧项目之外的新volume；固定镜像/model digest见Compose及ADR。索引只有20段，SQL精确余弦排序按chunk ID打破并列，不外推生产规模性能。业务服务镜像只复制runtime，不包含evaluation。40开发例已用于检查，不能重新称为未见保留集；20保留例未生成/打开。

## 验证、重启与清理

```powershell
$env:JOBFORGE_BUSINESS_TEST_DSN='postgres://jobforge_business_bootstrap:jobforge_business_bootstrap@127.0.0.1:5434/jobforge_business?sslmode=disable'
$env:JOBFORGE_TEST_PYTHON=(Resolve-Path .venv\Scripts\python.exe).Path
go test -race -count=1 -v ./internal/business ./internal/jsonstrict ./tests/integration/business
.venv\Scripts\python -m pytest python/tests
.venv\Scripts\mypy python/jobforge_agent
docker compose -f deploy/compose.agent.yaml restart business-postgres
docker compose -f deploy/compose.agent.yaml up -d --wait business-postgres
docker compose -f deploy/compose.agent.yaml restart business
Invoke-RestMethod "http://127.0.0.1:8092/business/v1/snapshots/$($snapshot.snapshot_id)" -Headers $read
```

PG测试创建随机命名的独立可重建测试数据库并在结束时删除；不清空演示库。普通测试未配置DSN或Python产生的skip不计通过，专门CI job显式配置并执行。合成向量仅测试数据库和HTTP机械契约；上面的真实prepare/retrieval才是模型与检索验收。完整仓库门禁仍见[开发指南](development.md)。本切片没有更改队列热路径，不新增或覆盖历史W4性能结论。

只停止本项目：`docker compose -f deploy/compose.agent.yaml --profile models down`。删除本项目可重建数据库和模型缓存：`docker compose -f deploy/compose.agent.yaml --profile models down --volumes`；之后必须重跑初始化。操作者输出目录自行保留或删除，不属于权威任务状态。

常见故障：模型不可用/digest不符会在发送embedding前失败；chunk超上下文由truncate=false返回明确失败；语料hash不符拒绝准备/装载；503检查DB用途标记、迁移版本、seed和发布索引；403表示reader调用操作者接口；409表示幂等键冲突或profile不符。不会用fixture兜底，也不记录完整工单、检索正文或鉴权值。取消只停止后续处理/本地等待，已发送的Ollama请求可能继续占用服务端资源。

远程模型、生产留存策略尚未验收；历史W4失败和AT-25跳过继续保留。本页不声称Run崩溃恢复、审批写入或完整Agent已交付。
