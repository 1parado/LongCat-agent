---
title: 'IM Bridge：远程消息不是第二个 Agent'
description: 'Feishu、Lark、微信等渠道如何被归一为 provider-neutral message，并复用本地会话控制。'
date: 2026-07-26
slug: im-bridge
order: 6
eyebrow: 'CHANNEL / IM BRIDGE'
tags: ['im', 'remote', 'session']
---

把 Agent 接到 IM 平台后，最容易出现的架构问题是：为每个平台复制一套 Agent 逻辑。LongCat-frontend 的方向相反：IM 只负责连接与翻译，消息一旦进入 Bridge，就复用本地 session、loop、skills 和 tool events。

## 渠道差异停在边缘

`internal/im/types.go` 定义了 provider-neutral 的对象：`RemoteChannelID`、`IncomingMessage`、`ChannelInstance` 和 `BridgeStatus`。当前枚举覆盖 Feishu、Lark、Weixin，也为 DingTalk、WeCom、Telegram、Slack、Discord、Matrix 留出了统一模型。

每一条输入都至少有这些字段：

```text
channel       来自哪个平台
user_id       谁发的
chat_id       哪个会话
text          消息内容
mentioned     是否提及机器人
```

适配器可以拥有自己的 webhook、扫码登录或凭据格式，但进入控制面以后，不再把平台字段泄漏给 Agent core。

## 控制命令和普通文本分开

`ParseControl` 使用一套 provider-neutral 的命令语法：

| 输入 | 含义 |
| --- | --- |
| `/p` / `/p demo` | 列出或选择项目 |
| `/r` / `/r 123` | 列出或恢复会话 |
| `/new` | 新建会话 |
| `/stop` | 停止当前任务 |
| `/whoami` | 查看当前身份 |
| `/help` | 查看帮助 |
| `cancel` / `0` | 取消当前选择 |

其他文本原样变成 `ControlUserMessage`。这一步非常朴素，却避免了“不同平台的斜杠命令各写一份”的长期分叉。

## 远程输入仍然是可见的

进入 session 前，消息会通过 `VisibleUserContent` 加上 `[Remote IM · channel]` 标记。它让模型和日志都知道这不是本地输入，同时保留用户原文。

真正的请求随后走同一个 `AskWithAttachments`：

```text
IM adapter → IncomingMessage
           → ParseControl
           → visible user content
           → Session.AskWithAttachments
           → loop / tools / events
           → bridge sends reply back to chat
```

这条路径带来一个很实际的好处：本地 Web UI 看到的工具事件与远程 IM 触发的工具事件来自同一个 source of truth。

## 远程通道更需要策略

`ACLConfig`、`ProjectScope`、`PresenterMode` 说明 Bridge 不是简单的“把 webhook 接到聊天 API”。远程输入至少应该能表达：允许谁、允许哪个 chat、是否要求 mention、是否只在当前项目执行，以及是否分享 session。

凭据也应该只保存引用和状态，不把 secret 直接放进 UI 响应。IM 是把本地 Agent 的影响半径扩大了，所以它必须继承本地 workspace 和会话边界，而不是绕过它们。
