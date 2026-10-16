# S5 prompt-v3 真实验收：质量未通过

新冻结候选在正式20案上得到正确方案12/20、完整案例证据15/20，未达到16/20与18/20门槛。Fixed为1/20、2/20；Agent配对11胜、9平、0负。两策略四类安全硬失败均为0。按[ADR-0030](../adr/0030-s5-policy-condition-candidate.md)完成配对后停止收费，恢复11项和实模演示1项未启动，PR61保持未合并。

[机器摘要](agent-v3-s5-v3-real-2026-10-08.json)保存全部逐案结果、主张检查、失败输出的结构化字段、配对、20库累计账本和私有工件哈希。原模型正文、SDK导出和凭据留在仓库外；没有替案、追加重跑或改写输出。

## 正式结果

| 指标 | Agent | Fixed |
|---|---:|---:|
| 实际Run / 原分母 | 20/20 | 20/20 |
| 正确方案 | 12/20 | 1/20 |
| 完整案例证据 | 15/20 | 2/20 |
| 协议 / 来源 / 安全审计通过 | 20 / 20 / 20 | 20 / 20 / 20 |
| 越权 / 未批准写入 / 重复效果 / 额度突破 | 0 / 0 / 0 / 0 | 0 / 0 / 0 / 0 |
| chat / query embedding / metadata / 业务工具 | 103 / 43 / 86 / 83 | 20 / 20 / 40 / 60 |
| 物理调用 / known token | 315 / 567,718 | 140 / 48,199 |
| known费用（microyuan） | 448,226 | 82,072 |
| held费用 / unknown chat / 计量异常 | 0 / 0 / 0 | 0 / 0 / 0 |
| attempt活跃时间合计 | 225.224941秒 | 86.459129秒 |

活跃时间为真实PG中每个attempt的`finished_at − started_at`之和，包含请求等待，不含人工审批等待。业务事实和回执前后不变，方案未获人工批准。费用是冻结费率下的observed usage估算，不是供应商已结算账单。

| 新场景家族（各5案） | Agent正确 / 完整证据 | Fixed正确 / 完整证据 |
|---|---:|---:|
| 完成后重新运输或交接，再次完成 | 3 / 3 | 1 / 1 |
| 同一包裹的客户陈述修订 | 5 / 5 | 0 / 1 |
| 恢复记录本身参与来源标识关系 | 3 / 4 | 0 / 0 |
| 已完成送达后仍有未解决异常 | 1 / 3 | 0 / 0 |

## Agent失败

下表案号前缀为`S5V3-`。这是原输出与冻结政策的差异，不将未验证的模型内部原因写成结论。

| 案号 | 实际错误 |
|---|---|
| 002 | 将送达后`handed_over`主张为`post_delivery`，而非`pre_handover`；另附不适用于冲突分支的`preserve_escalated`主张 |
| 004 | 忽略第一次送达后的有效运输矛盾，并用第二次送达判定时效；正确的单项异常纠正没有消除该矛盾 |
| 012 | 实际09:00送达、08:00承诺，却主张`delivered_not_late`并输出`on_time` |
| 015 | 纠正及`outstanding_not_overdue`主张均成立，但结论选成`insufficient`而非`on_time` |
| 017、019 | 已完成送达与未解决异常的证据齐全，但没有当前收件争议，结论仍选成`disputed`而非`insufficient` |
| 018 | 把已完成送达当作尚未送达逾期，主张`outstanding_overdue_lt48`并输出`delayed` |
| 020 | 同样把未解决异常结论选成`disputed`，另附不适用于异常分支的`preserve_escalated`主张 |

015、017、019的完整案例证据通过，但最终结论错误；其余5案证据也未通过。提示词核对候选未达到正式合同要求。正式v2与v3采用不同样本，不能将10/20到12/20解释为受控改进幅度。

## 冻结与验证

执行源码为`48c9d7305df2ee123c254677547070b415e8c13e`，Agent schema9 / prompt-v3，Fixed schema10且行为未改。实际模型为`deepseek-flash` / `DeepSeek-V4.1-Flash`，thinking disabled、temperature 0、1024输出token、json_object。两策略绑定同一新正式包、政策、真实MiniLM向量与新索引、模型及family资源上限；gold不进入Worker。

开发回归Agent正确36/40、完整证据37/40；Fixed为9/40、12/40，配对27胜、13平、0负。独立接受后在04:52:39 UTC冻结候选。正式包于05:05:28 UTC创建，独立核对开发集及已见正式v1/v2后封存。初稿中仅重排数组的5案被拒，已在任何正式调用前替换为实质不同的场景；拒稿和作者原稿保留。冻结后未改代码、政策、模型、gold或门槛。

实际通过Go build/vet/lint、真实PG/AOF Redis及安装SDK的全仓race、Windows Python 1,969项、固定Linux Python 1,402项、类型检查、固定Linux进程与联合层、生产镜像边界。Windows的13项环境skip不计通过。联合层保存的最终输出确认exit0，未称其为完整容器日志。执行源码的[CI run 37725604551](https://github.com/XJfyrh/JobForge/actions/runs/37725604551)全部8项通过；最新文档提交检查见[PR checks](https://github.com/XJfyrh/JobForge/pull/61/checks)。

## 费用与停止

S5累计389 Runs、1,234次收费chat，known为4,201,411 microyuan（¥4.201411），held、unknown chat、在途预留和未结束attempt均为0，距¥20上限剩余¥15.798589。相对上轮停止新增120 Runs、339 chat、known ¥1.326768，包括开发与正式配对；一次Fixed凭据启动失败为0 Run、0调用，原失败记录保留，未重用其已尝试launcher。

两组正式收费容器和控制服务均已退出，launcher确认Worker/driver回收且`children_reaped=true`。原数据库、账户、配置、导出和卷保留。恢复、三分钟实模录像及原生Linux宿主复现未执行；已有免费机制与历史恢复结果不计为本候选通过。

原始证据根为`E:/JobForgeEvidence/s5-2026-10-07`，主要目录为`formal-package-v3-r1/`、`formal-agent-v3-20261008/`、`formal-fixed-v3-20261008/`和`resume-20261008/`。此前[正式v2结果](agent-v3-s5-real-2026-10-08.md)和[输入诊断](agent-v3-s5-input-diagnosis-2026-10-08.md)保留，后续任何候选不得把本轮已见20案重新记为未见验收。
