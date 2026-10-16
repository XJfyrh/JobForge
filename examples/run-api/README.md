# Run SDK 与 HTTP 请求合集

先启动[免费演示](../../docs/quickstart.md)，生产环境使用管理员实际登记的profile、预算及独立凭据。全部例子从仓库根目录执行；旧Job脚本`agent_rag.py`和`observability_acceptance.py`继续服务旧路径。

## SDK

按Quickstart一次配置`JOBFORGE_API_URL`、`JOBFORGE_API_KEY`、`JOBFORGE_RUN_PROFILE`和`JOBFORGE_RUN_BUDGET_BATCH`。

```sh
python examples/run_submit.py --ticket ticket-1 --request hello-run-1
python examples/run_review.py RUN_ID
```

审阅完整方案和hash后，换独立approver凭据：Bash执行`export JOBFORGE_API_KEY=demo-approver`，PowerShell执行`$env:JOBFORGE_API_KEY='demo-approver'`。

```sh
python examples/run_review.py RUN_ID --decision approve --proposal-hash REVIEWED_HASH --request approve-hello-run-1
python examples/run_effect.py RUN_ID
```

拒绝用`--decision reject`；同一次决定保留原`--request`及hash。核对只由operator显式执行`python examples/run_effect.py RUN_ID --reconcile`，查询既有回执，不重发写入或模型。正常批准可能尚在执行，稍后显式再查effect。

## curl

以下Bash示例中的`RUN_ID`和`REVIEWED_HASH`来自上一步实际响应；这些请求独立执行，不自动重试。

```sh
curl --fail-with-body http://127.0.0.1:8195/v2/identity -H 'Authorization: Bearer demo-operator'
curl --fail-with-body http://127.0.0.1:8195/v2/runs -H 'Authorization: Bearer demo-operator' -H 'Content-Type: application/json' -H 'Idempotency-Key: submit-curl-hello-1' --data-binary @examples/run-api/submit.json
curl --fail-with-body http://127.0.0.1:8195/v2/runs/RUN_ID -H 'Authorization: Bearer demo-reader'
curl --fail-with-body http://127.0.0.1:8195/v2/runs/RUN_ID/approval -H 'Authorization: Bearer demo-approver'
curl --fail-with-body http://127.0.0.1:8195/v2/runs/RUN_ID/steps -H 'Authorization: Bearer demo-reader'
curl --fail-with-body http://127.0.0.1:8195/v2/runs/RUN_ID/effect -H 'Authorization: Bearer demo-reader'
```

批准的请求体必须由审阅者填入实际原hash，保存为仓库外的`approval.json`，内容为`{"schema_version":1,"decision":"approve","proposal_hash":"REVIEWED_HASH"}`，然后发送：

```sh
curl --fail-with-body http://127.0.0.1:8195/v2/runs/RUN_ID/approval -H 'Authorization: Bearer demo-approver' -H 'Content-Type: application/json' -H 'Idempotency-Key: approve-curl-hello-1' --data-binary @approval.json
curl --fail-with-body -X POST http://127.0.0.1:8195/v2/runs/RUN_ID/reconcile -H 'Authorization: Bearer demo-operator' -H 'Content-Type: application/json' --data '{"schema_version":1}'
```

Windows PowerShell使用`curl.exe`执行上述单行请求；不把`curl`别名当作curl二进制。也可使用原生请求：

```powershell
$headers=@{Authorization='Bearer demo-operator'; 'Idempotency-Key'='submit-ps-hello-1'}
$body=Get-Content -LiteralPath examples/run-api/submit.json -Raw
$accepted=Invoke-RestMethod http://127.0.0.1:8195/v2/runs -Method Post -Headers $headers -ContentType application/json -Body $body
Invoke-RestMethod "http://127.0.0.1:8195/v2/runs/$($accepted.run.run_id)" -Headers @{Authorization='Bearer demo-reader'}
```

## 失败与不确定结果

| 响应 | 处理 |
|---|---|
| 401 | 凭据失效，重新连接，不自动重发写入 |
| 403 | 当前角色无权执行；operator不拥有审批权 |
| 409 | 检查业务键、原内容、方案hash和工单版本，不换键绕过冲突 |
| 410 | 内容/原请求过窗，不据此创建替代历史请求 |
| 429 | 显式决定后续查询策略，SDK不后台重试 |
| 超时/5xx | 可能已受理，保留原业务键/操作键，查询原Run或回执 |
| HTTP 200且Run failed | 执行失败，读取`run.error`；HTTP成功不等于业务完成 |

示例请求与响应遵守[OpenAPI](../../api/run/v2/openapi.yaml)。原状态与独立效果分开；[unknown/held](../../docs/glossary.md)不能解释为未发送、未写入或零费用。
