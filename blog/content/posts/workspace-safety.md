---
title: 'Workspace safety：Agent 能写，但不能越界写'
description: '文件读写、symlink escape、diff 和 undo 如何把模型动作锁在当前工作区内。'
date: 2026-07-25
slug: workspace-safety
order: 7
eyebrow: 'RUNTIME / WORKSPACE'
tags: ['workspace', 'safety', 'undo']
---

Agent 最有价值的能力往往也是最危险的能力：读文件、写文件、批量修改。LongCat-frontend 的安全假设不是“模型不会犯错”，而是“即使模型犯错，工具边界也应该拒绝越界动作”。

## Workspace 是每个文件工具的前置条件

`ToolExecutor` 持有当前 `Workspace`。`ValidateWorkspace` 先把路径转成绝对路径，确认它存在且确实是目录；之后的 `list_directory`、`read_file`、`write_file`、`preview_file` 都在这个根目录下解析相对路径。

理想的安全路径应当满足：

```text
requested path
    ↓ clean + absolute
workspace root + relative path
    ↓ verify containment
regular file / directory operation
```

仅仅使用 `filepath.Join` 不够，因为 `..` 和 symlink 都可能让一个看似相对的路径逃出 root。项目现有工具测试也围绕 workspace 限制和链接逃逸做回归保护，修改工具时不能把这些检查删掉。

## 写入不是黑盒副作用

写入工具会创建父目录，并把变更交给工作区变更记录。服务层还能通过 Diff API 把变更呈现给用户；UI 因此不需要猜测 Agent 改了什么。

在一个实际的 frontend workflow 里，用户关心的不是“模型说它写完了”，而是：

- 哪个文件变了？
- 改了几行？
- 这次修改是否可以撤销？
- 我能不能先预览再继续？

这也是为什么 preview、diff、undo 都是原生工具，而不是靠模型在回答里描述一个“建议的 patch”。

## Undo 是状态模型的一部分

`internal/workspace/undo.go` 保存可回滚的快照，`changes.go` 和 `diff.go` 负责把文件状态变化结构化。Agent loop 每次执行工具后都会继续得到结果，但用户仍然拥有恢复入口。

因此本地 Agent 的权限可以被描述为：

```text
模型：选择一个已声明的动作
工具：在 workspace 内执行动作
工作区：记录变化并提供回退
用户：观察、接受、撤销
```

这条链上的任何一层都不应该只相信自然语言。自然语言是意图，文件系统状态才是事实。

## 安全也是体验

拒绝越界路径时，错误应该告诉模型和用户发生了什么，而不是返回一个模糊的“操作失败”。清晰的错误会进入下一轮上下文，模型可以换成合法路径；用户也能知道为什么某个动作没有发生。

要继续阅读实现，可以从 `internal/agent/tools.go` 的路径解析辅助函数开始，再对照 `internal/workspace/diff.go` 和 `internal/workspace/undo.go` 的测试。安全边界只有在失败用例里才真正可见。
