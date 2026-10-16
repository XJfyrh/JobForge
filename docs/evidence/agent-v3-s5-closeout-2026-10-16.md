# S5 最终执行与质量结果

工程能力已完成，V9正式模型质量达标，实际人工审批与业务提交后丢失响应的恢复演示已完成。最新归档提交的CI和PR交付状态统一见[当前状态](../status.md)，验收合同逐项对应[原S5矩阵](../agent-v3/s5-acceptance.md)。本报告记录实际结果，不扩大阶段范围。

| 验收内容 | 实际结果 | 复核入口 |
|---|---|---|
| 未见正式质量 | Agent正确18/20、完整证据18/20，达到原16/18门槛；硬失败0。Fixed各11/20，胜8/平11/负1 | [全部20案及两段Fixed来源](agent-v3-s5-v9-formal-v4-2026-10-16.md) |
| 步骤恢复与成本 | 原11项均执行，5项真实恢复通过；8个完整方案中7个正确，3个H0计划取消。两组可比C费用更高，另一组错答不可比 | [原分母、费用、耗时与失败](agent-v3-s5-v9-recovery-2026-10-16.md) |
| 真实业务动作恢复 | 具体方案获维护者批准后写入一次；真实SIGKILL/Wait、自然lease/Sweep、新attempt只查询回执，同一Run成功；模型无重放 | 下文实际Run及机器摘要 |
| 页面、Trace、留存与备份恢复 | 同源页面、权限、14面板、collector故障、内容清理及双库42表恢复；开发/已见/正式140Run/790普通HTTP的原OTLP证据继续有效，本案另核7普通调用及动作恢复 | [免费机制](agent-v3-s5-free-2026-10-07.md)、[机器摘要](agent-v3-s5-closeout-2026-10-16.json) |
| 费用闭账 | 50库累计727Run/2,924chat，usage估算¥23.325083；原¥28剩余¥4.674917。held、unknown chat、在途与未结束attempt均0，本批容器停止，锁释放 | 机器摘要绑定原PG账本与独立收据 |

实际演示Run为`f26f2b0f-8a75-44ec-bd7c-6869e1bc1665`，操作为`49392168-0b45-485e-9e2a-33ee3a9c923a`。DEV-002原方案通过同一评分器：结论`on_time`、动作`record_conclusion`、工单状态`open`。维护者对该Run及完整proposal hash明确回复“批准该方案并继续演示”，审批者为`s5-demo-maintainer-20261016`。业务库唯一结论记录在响应被丢弃前已经存在；恢复前后回执完全相等，工单版本2→3，没有第二次写入。

本案4chat、3普通业务HTTP及3动作HTTP（初始查回执、唯一POST、恢复GET）分开计数，新增usage估算¥0.039035。原丢ACK的POST记录仍为`unknown`，不伪造已观测响应；最终效果由恢复GET与实际SQL唯一回执确认为`applied`。这与费用unknown chat为0不同。SIGKILL的attempt2可能没有已结束根span，真实PG、Wait和物理派发原件保留；已结束attempt1/3都经实际`run.http → sdk.run.submit`父子路径关联原提交，恢复Trace明确为GET。

三分钟视频位于仓库外`E:/JobForgeEvidence/s5-2026-10-07/s5-closeout-20261016/demo/s5-three-minute-demo.webm`，附同名`.vtt`字幕和`video-assembly-receipt.json`。它是原速实际片段剪辑：0–75秒审批/故障、75–140秒闭账后关闭执行profile的只读回看、140–180秒实际Jaeger回看；成片不代表端到端耗时，完整准备与人工等待另见原始时间戳和录像。SDK提交到方案首段录制失败、原文件0字节，明确保留，不计成功录像；该段由原SDK回执、质量报告和实际Trace核验，没有为补录像新增模型调用。原有效录像、最终页面截图及所有剪辑来源SHA均保留。

环境为Windows Docker Desktop上的独立Linux Compose项目，固定镜像、SDK、实际MiniLM及业务PG；另有Linux CI真实进程/race检查。没有另称在第二台原生Linux宿主完成实模演示。费用是实际usage按冻结费率的估算，不是供应商结算账单。

[机器摘要](agent-v3-s5-closeout-2026-10-16.json)绑定启动审查、实际人工批准、原SDK/PG/回执、Trace、独立结果收据和视频字节。历史错答、停止批次及一次1Run/零chat的撤销启动均保留，不因本轮通过改写历史。验收无需新密钥、服务器或追加费用；可从上表逐项复核，最新CI与交付事实从状态页进入。
