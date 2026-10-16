# 开始一个免费 v3 Run

本体验运行真实Go控制/Worker、隔离PostgreSQL、业务HTTP和固定Python进程。**模型响应、向量和账本费用为模拟**；它证明接入及审批链路，真实模型质量见[V9报告](evidence/agent-v3-s5-v9-formal-v4-2026-10-16.md)。不需要云端密钥。

## 启动与页面体验

需要Git和Docker Compose，Docker使用Linux容器。从仓库根目录执行，Bash和PowerShell相同：

```sh
docker compose -f deploy/demo/compose.yaml up -d --build --wait
```

首次下载和构建需要额外时间；镜像已准备后的体验目标为5分钟，实测见[演示说明](demo.md)。默认项目为`jobforge-hello`，仅发布本机8195端口。冲突时Bash使用`export JOBFORGE_DEMO_PORT=18195`，PowerShell使用`$env:JOBFORGE_DEMO_PORT='18195'`后再启动。

打开`http://127.0.0.1:8195/ui/`：

1. 用`demo-operator`连接，点击新建任务。
2. 输入工单`ticket-1`，选择“免费机制演示”，提交一次；保存完整任务编号，刷新查看方案。
3. 查看方案及来源。模型响应为脚本化结果，不作为模型推理效果展示。
4. 退出，用独立`demo-approver`连接，核对原方案与影响后批准或拒绝。
5. 刷新确认实际更新；需核对回执时使用operator。`demo-reader`只读。

固定凭据属于本机隔离演示，不能用于生产。内部网络无互联网出口，PG不向宿主发布。该镜像是明确的integration target，生产镜像不包含演示适配器。

## 用SDK创建Run

需要Python 3.10+，从仓库根目录安装：

```sh
python -m pip install ./sdk/python
```

Bash配置：

```sh
export JOBFORGE_API_URL=http://127.0.0.1:8195
export JOBFORGE_API_KEY=demo-operator
export JOBFORGE_RUN_PROFILE=approval-joint-synthetic-v1
export JOBFORGE_RUN_BUDGET_BATCH=contract-batch
```

PowerShell配置：

```powershell
$env:JOBFORGE_API_URL='http://127.0.0.1:8195'
$env:JOBFORGE_API_KEY='demo-operator'
$env:JOBFORGE_RUN_PROFILE='approval-joint-synthetic-v1'
$env:JOBFORGE_RUN_BUDGET_BATCH='contract-batch'
```

执行：

```sh
python examples/run_submit.py --ticket ticket-1 --request hello-run-1
```

脚本只提交一次，输出Run ID；最多轮询60秒，每次HTTP超时10秒。网络未确认时保留原`--request`，重试复用原业务意图。审批和回执操作见[SDK/curl合集](../examples/run-api/README.md)。

## 停止与排查

```sh
docker compose -f deploy/demo/compose.yaml logs --tail=80 demo
docker compose -f deploy/demo/compose.yaml stop
```

会话最长90分钟。正常停止回收本次创建的专用库，PG卷保留；重新启动创建新会话，旧任务不作为演示存档。命令只操作此Compose项目。

构建失败检查Docker Linux模式与网络；启动失败查日志和端口占用。方案失败应保留Run ID及错误码。真实模型接入另见[部署参考](deployment.md)、[业务边界](support-overview.md)和[有界批次](agent-v3/cloud-batch.md)。
