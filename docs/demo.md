# 三分钟看懂 JobForge

一个售后问题怎样成为有依据的处理方案，又怎样经过人工批准保存到工单？视频沿新版工作台走完这个过程，并展示业务提交后丢失响应时的恢复结果。

[观看三分钟视频](https://github.com/XJfyrh/JobForge/releases/download/v0.1.0-preview.1/jobforge-demo.mp4) · [下载视频、双语字幕与来源清单](https://github.com/XJfyrh/JobForge/releases/tag/v0.1.0-preview.1)

[![三分钟演示封面，沿用新版工作台风格](images/demo-cover.png)](https://github.com/XJfyrh/JobForge/releases/download/v0.1.0-preview.1/jobforge-demo.mp4)

## 视频里看什么

| 时间 | 内容 | 素材类型 |
|---|---|---|
| 0–20 秒 | 认识售后处理闭环 | 标题卡 |
| 20–80 秒 | 查看方案、订单、物流引用和政策 | 原真实 V9 结果在新版 UI 的只读回看 |
| 80–125 秒 | 切换独立审批身份，批准后核对工单更新 | 隔离免费演示，模型响应、向量和金额模拟 |
| 125–160 秒 | 业务提交后丢失响应，接管后确认唯一回执 | 原真实故障录像及结果回看，保留旧 UI 标识 |
| 160–180 秒 | 查看验收结果与接入入口 | 标题卡 |

片段为原速节选，按章节展示。完整原片、Run 标识、SHA-256 和录制过程见[来源记录](evidence/public-readiness-2026-10-16.md#录像来源)。

![真实 V9 结果在新版工作台中的只读回看](images/run-result.png)

## 自己试一次

按[免费 Run](quickstart.md)启动隔离环境，提交工单、查看方案、切换审批身份并核对回执，无需云端密钥。镜像准备好后，目标为 5 分钟完成体验；首次下载和构建时间另计。[实测记录](evidence/public-readiness-2026-10-16.md#免费体验测量)包含 Windows Docker Desktop 的两次耗时及 Linux CI 入口。

了解 Agent 的职责与人工介入条件，继续看[业务说明](support-overview.md)；理解故障恢复设计，看[工程亮点](engineering.md)。
