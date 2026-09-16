# JobForge PRD v0.11：正式执行器的确认与终止边界

- 日期：2026-09-16；状态：Proposed，等待PR审查。本文不表示已实现或已验收。
- 基线：[PRD v0.10](JobForge_PRD_v0.10.md)、[ADR-0018](../adr/0018-deepseek-fixed-flow-and-executor.md)。实现调查见[受控HTTP模块](../agent-v3-authorized-http.md)。
- 决策提案：[ADR-0019](../adr/0019-executor-confirmation-and-exit-contract.md)。只细化C-02/C-03/C-08正式进程接缝，不扩大业务能力或改变费用上限。

## 1. 问题与行为

Python调用模块必须等待控制面确认一次物理调用的观察事实，才能生成下一调用或步骤结果。现有内部v2只有observation，没有对应确认帧；管道写成功、Go解码成功和计量结算都不能证明ObserveCall已提交。正式执行器增加绑定原调用和观察内容的明确ACK，免费、unknown计量及最终一次调用同样等待确认。ACK不授予新HTTP许可、不续期、不提交步骤。

普通执行会话已关闭时，大小越界等失败不能伪装成可纠正的模型输出。固定step/guardian使用少量固定退出码向Go报告终止事实，Go结合已锁存的停止原因、标准协议、实际Wait和执行权裁决。stderr不作为错误协议。任何退出码都不能单独触发成功、退费或重发。

Go保持唯一lease/心跳/RPC所有权，Python只执行预注册单步。终止宽限仍100ms；Kill/Wait不被计量RPC阻塞。只对已完整收到的原调用计量执行独立总计≤2秒收尾。成功结果要在旧组消失、各reader完成有限排空/Join、无尾随协议错误后才能Commit；Python发结果后退出，不等待Go的Commit ACK。

## 2. 验收映射

| ID | 要求 | 实际验证条件 |
|---|---|---|
| C3-01 | 观察持久确认 | 真实ObserveCall提交前阻塞/提交后丢ACK时，下一HTTP均为0；错身份/hash、重复、迟到ACK拒绝；免费及unknown也适用 |
| C3-02 | 唯一顺序与期限 | Go/Python共同fixture与实际双向管道验证许可→发送→计量/观察确认→结果；等待不延长原截止，旧无ACK序列明确拒绝 |
| C3-03 | 类型化失败 | 实际子进程大小/输入/身份/依赖/超时/协议失败正确归类；Go已知预算拒绝不被通用退出覆盖；无已知STOP的本地停止不再续Run租约；信号/未知退出码不冒充provider事实，大小/超时/停止不进入纠正 |
| C3-04 | 终止与计量独立 | 普通坏帧、EOF、取消及父/guardian死亡时实际Kill/Wait；只允许标准解码的完整原调用计量有界补报，晚到确认不重开执行 |
| C3-05 | 提交屏障 | 只有标准结果、确认链、EOF/Join、Wait、组消失、stderr边界及当前执行权同时成立才Commit；尾随帧/缺回执/组残留不Commit |
| C3-06 | 唯一部署合同 | 固定Go/Python镜像与不可变executor_version/profile hash匹配；拒绝混用旧v2实现；无第二种兼容模式或Python调度队列 |

先实现共同协议和确定性模块，再实现正式Linux supervisor/Go Worker，最后用真实PostgreSQL/gRPC/进程及合成HTTP故障服务联合验证。合成供应商只验执行机制，不能计为真实DeepSeek、检索质量或40案通过。Windows执行同一Linux容器，不能用不支持分支的测试替代。

## 3. 范围与保留限制

协调更新尚未正式部署的内部`api/executor/v2`，通过新的executor_version/profile hash拒绝混合版本；明确这是内部序列的不兼容细化。公开HTTP、SDK、gRPC及PostgreSQL可靠性语义保持兼容。v1历史合同不变，不设计在途正式v2任务迁移，因为当前尚无正式执行器或此类任务。

本增量不交付新的support策略/方案评分，不添加provider审计存储或跨队列trace持久字段。这些缺口在真实C-01/C-04～07及完整观测验收前另行补齐；C2的内存audit与HTTP traceparent不能代替。5 CNY首批额度、40例全分母、unknown全额hold、at-least-once和业务幂等均保持。

历史W4性能门禁失败、AT-25跳过、远程模型与生产留存未验收继续保留。每个实现PR报告实际通过、失败、跳过和未运行层次；整体S1及S2～S5不因协议合并而完成。
