---
title: 'Skills：把长提示词变成按需加载的知识包'
description: '项目技能、用户技能、关键词匹配和 load_skill，如何把上下文成本控制在需要的地方。'
date: 2026-07-28
slug: skills
order: 4
eyebrow: 'CONTEXT / SKILLS'
tags: ['skills', 'context', 'extensibility']
---

如果把所有“怎么写 React / 怎么审查 UI / 怎么做测试”的知识都塞进 system prompt，Agent 会变得昂贵、迟钝，而且很难更新。LongCat-frontend 把这些知识放进 `SKILL.md`，让上下文像文件系统一样可以被浏览和按需读取。

## Skill 是什么

`internal/frontend/skills.go` 中的 `Skill` 是一个轻量结构：

```go
type Skill struct {
    Name        string
    Title       string
    Description string
    Keywords    []string
    Body        string
    Path        string
}
```

加载时只读取每个 skill 的 frontmatter 和正文，解析出标题、描述、关键词。系统提示里默认只注入一个清单：有哪些技能，以及它们大概解决什么问题。

真正的正文通过 `load_skill` 工具读取。这是一种很重要的延迟：**目录信息进入模型，长文本只有在真的需要时才进入上下文。**

## 两个来源，项目优先

`NewSession` 会合并两类目录：

```text
frontend-skills/                  项目随仓库分发
~/.longcat-frontend/skills/       用户通过 Market 安装
                ↓
             []Skill
```

用户级 skill 同名时会在 `ReloadSkills` 中覆盖已有项；新 skill 则追加。这让项目可以带一套默认能力，个人又能在本地迭代自己的工作流，而不需要修改 Agent 核心代码。

## “相关”不等于“已加载正文”

每轮输入会经过 `frontend.Match` 做一个简单的关键词匹配，最多返回 3 个相关技能名称。UI 可以用这份结果显示“本轮命中了哪些能力”，但 system prompt 仍然要求模型在需要专门知识时调用 `load_skill`。

这两个层次分别解决不同问题：

- `Match` 是低成本的导航提示，不把大量内容自动塞入上下文。
- `load_skill` 是模型明确选择后的知识加载，正文内容可被完整复现。

## 为什么用 Markdown

Skill 不是 Go interface，也不需要编译。它本质上是一份面向 Agent 的操作手册：可以描述判断标准、代码模板、验证步骤和失败处理。Markdown 还有两个现实优点：

1. 可以被人直接审查、版本控制和 code review。
2. 能和前端项目的文档、规范、示例放在同一个工作流里。

对一个 frontend-focused Agent 来说，技能的价值不在于“插件化”这个名词，而在于它让专业知识从 runtime 中脱离出来，成为可替换的内容资产。

## 读源码时看这四个点

- `LoadSkills`：目录遍历与缺失目录的宽容处理。
- `parseSkill`：简易 frontmatter 的解析范围。
- `Match`：关键词匹配和数量上限。
- `ToolExecutor` 中的 `load_skill`：模型选择 skill 后正文如何返回。
