# 部署适用性、配置与运维边界

本页描述Agent v3。旧Job的可靠性及取消信号合同保持独立，见[旧路径](legacy/README.md)。先体验[隔离免费Run](quickstart.md)，再按下表评估接入。

## 适用性矩阵

| 场景或要求 | 当前范围 | 部署方工作 |
|---|---|---|
| 本地开发、隔离演示 | 已验证；默认生产Compose不启用收费profile | 依赖准备、隔离配置 |
| 有限预算、人工监督的评测 | 有冻结候选真实证据 | 具体环境的策略、角色及预算登记 |
| Worker中断后的Run恢复 | 已验证特定自然lease、checkpoint和回执路径 | 保持数据库/依赖可用，核对错误分类 |
| 停写双库备份、新库恢复 | 已完成有限演练，恢复默认禁用执行 | 停写窗口、备份空间、逐表与身份复核 |
| 在线跨库备份、PITR、数据库HA | 当前v3未交付和验收 | 独立架构与灾备演练 |
| 生产容量、长期Trace、外部告警闭环 | 尚未验收 | 真实负载、留存策略和告警演练 |
| 可用率、端到端延迟、RPO/RTO | 尚未建立当前v3目标 | 部署拓扑、测量口径与恢复目标 |

**服务等级声明：当前Agent v3尚未建立面向生产部署的可用率、端到端延迟及灾备恢复目标。已有指标与有限环境测量供部署评估使用；旧Job路径的既有合同继续有效。**

100次只读、33次CommitStep及自然恢复的有限测量见[S5矩阵](agent-v3/s5-acceptance.md)，不外推为生产SLO。

## 平台、资源与端口

| 平台 | 当前范围 |
|---|---|
| Linux Docker/Compose | 固定Linux执行器及CI验证路径 |
| Windows Docker Desktop Linux容器 | 已有实际演示，使用相同固定执行器 |
| 原生Windows Worker | 当前不支持固定Linux执行器合同 |
| macOS Docker等其他宿主 | 未完成同等验收 |

版本见[开发指南](development.md)。Worker登记容量为1；生产最低CPU/内存与整机容量基线待建立。免费演示使用一个PG实例中的独立控制/业务数据库、一个执行容器和固定HTTP转发容器；[资源与耗时记录](evidence/public-readiness-2026-10-16.md#免费体验测量)注明测量环境。

默认卷、网络和端口以[Compose](../deploy/compose.agent.yaml)为准，开发凭据只用于本机合成环境。生产需配置独立秘密、低权限身份、网络入口及TLS。免费演示的执行容器仅接内部网络，入口代理只转发到固定后端，不提供任意URL代理。

| 组件 | 默认宿主入口 |
|---|---|
| v3控制HTTP / RPC / PG | 8093 / 9093 / 5435 |
| 业务HTTP / PG / Ollama | 8092 / 5434 / 11436 |
| 免费演示UI/API | loopback 8195，JOBFORGE_DEMO_PORT可改；PG不发布 |
| v3 Prometheus / Grafana / Jaeger | 9094 / 3004 / 16687 |
| 控制 / Worker metrics | 容器内6063 / 6064，只用于内部抓取 |
| 旧Job测试PG | 5433，与v3库分开 |

## 配置入口

Quickstart已提供一套完整免费配置。正式部署按所属组件提供以下变量；角色和可执行profile仍由服务端登记决定。

| 组件与条件 | 配置及含义 | 缺省/来源 |
|---|---|---|
| 控制服务 | JOBFORGE_AGENT_DSN；JOBFORGE_AGENT_CONFIG | 必填DSN和部署JSON，含profile/Worker/预算/绑定 |
| 控制监听 | JOBFORGE_AGENT_HTTP_ADDR；JOBFORGE_AGENT_GRPC_ADDR | 原生命令默认127.0.0.1:8093、:9093；Compose显式配置 |
| 公共身份 | JOBFORGE_AGENT_API_KEYS | token→tenant/role/actor的秘密JSON，approver需稳定actor |
| Worker身份 | JOBFORGE_AGENT_WORKER_KEYS | 独立principal/token，不作公共API凭据 |
| 快照依赖 | JOBFORGE_AGENT_BUSINESS_URL；JOBFORGE_AGENT_BUSINESS_KEYS | 业务origin及租户capture token，不由任务提供 |
| Worker | JOBFORGE_AGENT_GATEWAY；JOBFORGE_AGENT_GRPC_TLS；JOBFORGE_AGENT_WORKER_CONFIG；JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE | 受信配置和独立凭据文件，格式见[运行时](agent-v3/runtime.md) |
| 业务服务 | JOBFORGE_BUSINESS_DSN；JOBFORGE_BUSINESS_KEYS | 必填reader DSN和token→tenant/role的秘密JSON；capture身份按[业务准备](agent-v3/business.md)配置 |
| 业务监听 | JOBFORGE_BUSINESS_ADDR | 原生命令默认127.0.0.1:8092；Compose显式配置 |
| 控制审批启用时 | JOBFORGE_AGENT_ACTION_SIGNING_KEYS；JOBFORGE_AGENT_ACTION_RECEIPT_KEYS | 签名seed及receipt reader秘密，仅控制持有 |
| 业务动作启用时 | JOBFORGE_BUSINESS_ACTION_DSN；JOBFORGE_BUSINESS_RECEIPT_DSN；JOBFORGE_BUSINESS_ACTION_KEYS；JOBFORGE_BUSINESS_ACTION_PUBLIC_KEYS | 四项同时配置或全省略；格式见[审批](agent-v3/approval.md) |
| OTLP启用时 | JOBFORGE_OTEL_EXPORTER；OTEL_EXPORTER_OTLP_ENDPOINT；JOBFORGE_OTEL_SAMPLE_RATIO | 默认不导出，端点与采样显式配置 |
| 指标 | JOBFORGE_METRICS_ADDR；JOBFORGE_AGENT_WORKER_METRICS_ADDR | 内部监听；Worker启动器显式传递 |
| SDK示例 | JOBFORGE_API_URL；JOBFORGE_API_KEY；JOBFORGE_RUN_PROFILE；JOBFORGE_RUN_BUDGET_BATCH | 当前角色凭据及实际登记参数；[请求合集](../examples/run-api/README.md)有完整演示值 |

秘密保留于环境或受保护文件，不输出到manifest、日志、截图或普通报告。停用profile不删除历史；空配置只能查询，不执行未登记任务。

## 时间窗口与恢复

| 事实 | 当前合同 |
|---|---|
| 执行身份 | lease 30秒、heartbeat 5秒、attempt 180秒；保持自然恢复时限 |
| Run与费用批次 | 由提交/登记截止时间约束，实验窗口不是通用默认 |
| 审批 | 原方案提交后1小时与Run deadline的较早者；批准/恢复不续期 |
| 新人工retry | 原业务请求7日窗口；已授权动作走回执优先路径 |
| 内容清理 | 首次终态至少7日且原批次到期；活动/待审批保留 |
| 回执及晚到计量 | 回执至少30日；原调用晚到计量30日窗口，unknown/hold保留 |
| 免费演示 | 最长90分钟；正常停止清理会话专用库，重启创建新环境 |

停写、备份空间、新库默认禁用执行与身份防重见[操作指南](agent-v3/operations.md)。取消不能撤回已有业务效果，unknown时核对原回执；[术语表](glossary.md)解释执行和费用。
