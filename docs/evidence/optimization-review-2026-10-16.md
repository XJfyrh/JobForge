# 优化意见核查与实施（2026-10-16）

依据当前工作区逐项核查，已修复成立的问题。金额、质量门槛、历史冻结 schema、manifest、原始证据及停止批次不变；没有调用收费模型。架构及公开契约说明见 [ADR-0040](../adr/0040-s5-evidence-and-validation-consistency.md)。

| # | 核查结论 | 实施结果 |
|---|---|---|
| 1 | 属实：正式部署未比较 freeze.images | 正式新批次比较 freeze/build/config 四个镜像；任一变化拒绝，错误为 FROZEN_IMAGES_CHANGED。seen 诊断保留历史来源语义 |
| 2 | 属实：门槛、金额存在独立副本 | 执行单源为 tools/support_evaluation/s5_policy.py，scorer/freeze/host/guard/recovery 共用；该文件进入现有 source hashes。旧 guard 的规则有意保留，不能用新 health 替换历史批次语义 |
| 3 | 重复及校验缺口属实；$id 不能证明原有生成器 | 新共享模板/显式差异共946行，重现原6025行的16份 schema，逐字节一致；Python检查 Draft 2020-12 与绑定schema原字节SHA-256的Go向量，Go检查严格解码、生产builder及全部dataset枚举。历史 support-agent.source.example.json 是 schema 2 示例，不应升级成 v9 |
| 4 | 当前实际漏收10个常规用例，另有2个人工恢复用例；不是14个常规用例 | 常规 Linux 分片由38增至48；go-test 用同源排除模式避免重复，分片必须全部PASS且无SKIP。两项 S5Restored 需要明确拥有的历史恢复副本和签名动作，显式列为人工验收，不假定CI能运行 |
| 5 | 属实：四个关键说明文件未选Python检查，schema JSON过度路由 | 四个明确路径和16份 profile-schema.json 选 python-lint；普通说明仍只做文档检查，其他未知API/契约保留保守路由。当前验收门槛及host金额另有一致性断言 |
| 6 | 属实：能力枚举和schema映射重复 | internal/run 统一运行时能力判定；BuildSupportProfile/SupportProfileHash共用字段构造和schema映射，input/conversation/manifest/approval/recovery/audit复用判定 |
| 7 | 属实：Agent bundle已在Agent工具守卫校验，随后重复读取 | 通用bundle块只服务非Agent；新call的可执行登记检查复用已读profile，保留重放/disabled语义 |
| 8 | 属实：批次屏障反复传输、解码、校验同一profile | 事务内每个profile读取并验证一次，逐chat只传ID/hash；每行仍验证绑定。未引入跨事务或可变profile的全局缓存 |
| 9 | 详情7请求、列表N+1属实；当前源码没有2秒轮询 | 默认详情4请求；展开账本再取calls/action-calls，展开执行区再取steps；缺审批的已存方案仍取steps。每页预算3查询替代每项6查询，列表总查询从20项121次降为4次，保持repeatable-read |
| 10 | 全量聚合属实；添加state索引并不能直接证明全量计数变快 | 完整PG快照按15秒缓存并合并刷新，成功/失败均缓存；刷新失败不返回旧值或部分数据。动态指标改NewConstMetric错误路径；未新增未经EXPLAIN验证的索引或migration |
| 11 | 属实：runtime/provider-audit版本说明落后 | 补schema18、prompt-v9、audit-v2与manifest对应关系，并说明历史schema2示例的适用范围 |
| 12 | 属实，且mode_state也漏thinking | OpenAPI允许audit schema1/2及thinking，thinking强制schema2；新增Go生成的thinking fixture及SDK/schema正反例 |
| 13 | 多项属实；manifest“字段数量恒无效”的判断不成立 | 显式处理checkpoint解码错误；统一本地/CI命令和linter版本；诊断runner注册SIGTERM；预检使用显式数据库清单和证据所在卷。recoveries保留gauge并用delta告警，step kind改snake_case。保留manifest字段形状守卫，并补null/空prompt_version拒绝回归 |

## 测量范围

- 同一Windows主机、Go1.26、同一40-chat/1-profile输入的CPU微基准：逐chat解码/审计策略校验18.071ms、6,943,831B；按profile一次校验0.470ms、185,040B。此项只测屏障的profile装载工作，不代表完整事务、锁占用或端到端吞吐提升。
- 独占PG16、23个本租户Run及1个外租户Run：两页List与Get的三层预算、未知hold及已知结算一致。20个Run重复10轮：逐项Get共1.463s，按页List共82.9ms；这是两条查询路径的参照测量，不是旧List的精确前后基准。
- 真实浏览器配合明确标注的合成HTTP数据：默认4详情请求；展开账本增加2个请求，展开执行区增加1个请求，重复展开不重取。退出后隐藏详情并清空已加载内容。

## 验证记录

- 全仓 `go test -race -count=1 ./...`：真实独占PG/Redis/pgvector环境通过；Windows专用环境以外的skip不计Linux验收。新增查询回归及后续改动的包race另验。
- Python全套初次运行2566 passed、13 skipped；跳过为环境/专用开关，不计实际业务或云端通过。新增断言与后续修改按差异补跑。
- Ruff、mypy、SQLFluff、文档链接、Buf lint/breaking、Prometheus配置与告警规则、生产/外部operator镜像来源与边界检查通过。
- 固定Linux process通过；runtime core 43、recovery 3、deadlines 2个顶层用例全部PASS且无SKIP，保留真实30秒lease/180秒attempt。人工历史恢复用例未运行，收费runner未做live信号验证。
- 独立S0探针与共享BOOTTIME检查通过；固定Linux Python全套1508 passed，补齐Windows运行中的8个执行器环境skip。其余5个离线工具专用环境skip不计业务通过。
- 初次quick通过199项测试后，被原有 `.pytest_tmp/` 临时脚本阻断Ruff；按维护者要求忽略该目录后，默认quick已完整通过（201项Python测试、Ruff及internal/cmd/tools的Go测试）。临时文件保留。
- 以上记录提交前本地验证；云端CI与合并状态以对应PR最新head的CI required为准。
