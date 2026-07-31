---
title: 'LongCat / Engineering Notes'
description: '把模型输出变成可观察行动：LongCat-frontend 的架构笔记。'
---

LongCat-frontend 是一个 Go + Tauri v2 的轻量级 Agent 工作台。这里不写“AI 很神奇”，只记录它如何把一次自然语言请求变成一串可检查、可撤销、可继续的工程动作。

这是一组从源码出发的拆解：每篇文章只回答一个机制如何进入系统、如何穿过边界，以及它为什么这样设计。
