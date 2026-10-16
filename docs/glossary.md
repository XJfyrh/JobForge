# Run 与费用术语表

| 术语 | 含义与操作影响 |
|---|---|
| Run | 持久业务执行实例，不是一次模型HTTP请求 |
| attempt | 一次持有执行权的执行尝试；恢复产生新attempt，费用和家族限制继续累计 |
| profile | 固定策略、资源、模型、版本与额度的不可变登记 |
| lease | 执行租约；超期后旧进程失权 |
| fence / fencing token | 递增执行身份，与Run、owner、attempt和session共同校验 |
| checkpoint | 持久提交的步骤前缀，不保存模型内部推理进度 |
| held / hold | 预留或未知调用仍占用的保守额度，不是结算费用或可退款余额 |
| known | 按可信用量与冻结费率确认的估算，不是供应商结算账单 |
| unknown调用 | 请求/观察未确认，不证明未发出 |
| unknown计量 | 缺少足以定价的计量，可能继续保留完整hold |
| unknown效果 | 尚不能确认是否写入，应核对原回执而非直接重做 |
| ACK | 持久确认；观察、计量、步骤ACK各确认不同事实，断连不证明数据库未提交 |
| receipt / 回执 | 业务库按稳定operation保存的效果证明，不是模型文字承诺 |
| sweep | 扫描超期身份并推进恢复/结束，不恢复旧Worker权限 |
| gold | 离线标准答案，不进入生产镜像、模型输入或策略 |
| no_action | 已提交的明确无需动作，不等于缺结果或失败 |
| observed usage | 供应商观测计数，需核对模型与费率后才能用于known账本 |

family、tenant、batch是重叠预算范围，不能相加当成总支出。排障见[调用账本](agent-v3/runs.md#调用预算与故障判断)、[供应商审计](agent-v3/provider-audit.md)、[人工审批](agent-v3/approval.md)。
