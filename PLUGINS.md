# 插件 / 扩展机制（Plugin System）

LongCat-frontend 内置一套轻量、可移植的插件机制，用于在不修改核心代码的前提下
扩展能力。对应 ISSUES.md 的 **#5 没有 plug-in / extension 机制**。

## 设计原则

- **声明式、可移植**：插件是一个文件夹 + 一个 `plugin.json` 清单，统一挂载
  现有的扩展点（技能、子 Agent、MCP 服务）。
- **不依赖 cgo / 动态链接**：Go 的 `plugin` 包在 Windows 不可用，且 Tauri 打包为
  单一二进制；因此用清单驱动而非原生代码加载。
- **完整生命周期**：发现（Discover）→ 注册（LoadInto）→ 启用/禁用
  （Enable/Disable）→ 热重载（Reload）。

## 插件目录

| 作用域   | 路径                                          | 说明                       |
| -------- | --------------------------------------------- | -------------------------- |
| 用户级   | `~/.longcat-frontend/plugins/<plugin-id>/`    | 所有项目共享，推荐安装处   |
| 项目级   | `<项目>/.longcat-frontend/plugins/<plugin-id>/` | 随仓库走，团队共享       |

## 目录布局

```
<plugin-id>/
  plugin.json              # 必填：插件清单
  skills/<name>/SKILL.md   # 可选：前端技能（同 frontend-skills 格式）
  agents/<name>.md         # 可选：子 Agent 定义（同 agents 格式）
```

## plugin.json 字段

```json
{
  "id": "hello",
  "name": "Hello Plugin",
  "version": "1.0.0",
  "description": "示例插件：贡献一个技能与一组 MCP 工具",
  "author": "your-name",
  "disabled": false,
  "skills": ["skills"],
  "agents": ["agents"],
  "mcp": [
    { "id": "hello-svc", "name": "Hello Service", "url": "http://127.0.0.1:9000/mcp", "protocol": "http" }
  ]
}
```

| 字段         | 类型            | 说明                                            |
| ------------ | --------------- | ----------------------------------------------- |
| `id`         | string          | 唯一标识；缺省时取文件夹名                      |
| `name`       | string          | 展示名                                          |
| `version`    | string          | 版本（建议语义化版本）                          |
| `description`| string          | 简介                                            |
| `author`     | string          | 作者（可选）                                    |
| `disabled`   | bool            | 是否默认禁用，默认 `false`（启用）             |
| `skills`     | []string        | 相对插件根目录的技能目录，含 `SKILL.md` 子目录  |
| `agents`     | []string        | 相对插件根目录的 Agent 定义目录，含 `*.md`      |
| `mcp`        | []MCPServer     | 内联 MCP 服务；仅内存注册，**不写**项目 mcp.json |

插件声明的 MCP 服务通过 `mcp.Manager.AddEphemeral` 在会话内挂载，工具在后台异步
发现，不会对项目配置产生副作用。

## 生命周期

- **发现**：`plugin.NewManager(userDir, projDir).Discover()` 扫描两个目录下的
  `plugin.json`。
- **注册**：`Manager.LoadInto(session)` 将生效插件的技能 / 子 Agent / MCP 服务
  合并进 `agent.Session`。
- **启用 / 禁用**：`Manager.Enable(id)` / `Manager.Disable(id)` 维护
  `~/.longcat-frontend/plugins/disabled.json`，重启后保持。
- **热重载**：`Manager.Reload()` 重新发现并刷新状态。

会话启动时（`cmd/LongCat-frontend/main.go` 的 `newSession`）会自动发现并加载插件，
加载失败仅告警、不阻断主流程。

## 在 TUI 中查看插件

启动 TUI 后输入：

```
/plugins            # 列出已安装插件及其状态
/plugin enable <id> # 启用插件
/plugin disable <id># 禁用插件
```

> 注：启用 / 禁用后需重启会话（TUI 或 `serve`）才会重新加载；`/plugins` 仅展示
> 当前会话启动时的发现结果。

## 创建你的第一个插件

```bash
# 用户级插件目录
mkdir -p ~/.longcat-frontend/plugins/my-plugin/skills/demo
cat > ~/.longcat-frontend/plugins/my-plugin/plugin.json <<'JSON'
{
  "id": "my-plugin",
  "name": "My Plugin",
  "version": "0.1.0",
  "description": "我的第一个插件",
  "skills": ["skills"]
}
JSON
cat > ~/.longcat-frontend/plugins/my-plugin/skills/demo/SKILL.md <<'MD'
---
name: Demo Skill
description: 演示技能，匹配关键词 demo
keywords: demo, 示例
---
当需要演示时，展示一段示例回复。
MD
```

重启 LongCat 后，`/plugins` 即可看到 `my-plugin`，且输入含 “demo” 时会命中该技能。

## 扩展点路线图

当前已支持：技能、子 Agent、MCP 服务。后续可扩展（保持声明式、避免任意命令执行）：

- 自定义斜杠命令（在 `plugin.json` 声明 `commands`）。
- 工具调用前后钩子（在 `plugin.json` 声明 `hooks`）。
- 主题 / 提示词片段注入。
