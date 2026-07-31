# LongCat-frontend 已知问题与待办清单

> 记录项目当前存在的技术债与待处理事项。状态均为 **待处理（Open）**，除非特别标注。
> 本文档由代码核查得出，关键结论均附带文件/行号作为证据。

---

## 1. 缺少 CI/CD 配置

**现状**

仓库根目录下不存在 `.github/workflows`、`.gitlab-ci.yml`、`.circleci` 或其他任何 CI 配置。

```text
$ ls -la .github .gitlab-ci.yml .circleci   → 均不存在
```

项目为 Go + Tauri 工程，本地构建依赖 `gofmt` / `go test ./...` / Tauri 编译，但没有任何自动化门禁，提交质量完全依赖人工。

**影响**

- 主分支随时可能被未通过编译/测试的提交破坏。
- 贡献者无法从 PR 得到自动反馈（构建、lint、测试、安全扫描）。
- 发布（二进制 / Tauri 安装包）缺少可复现的流水线。

**建议**

- 至少补齐 GitHub Actions：`build`（跨平台 `go build`）、`test`（`go test ./...` + `gofmt` 校验）、`lint`。
- 后续补充 Tauri 桌面构建矩阵（Windows/macOS/Linux）与发布 workflow。

---

## 2. 缺少 CONTRIBUTING.md

**现状**

仓库内不存在 `CONTRIBUTING.md`（或 `CONTRIBUTING.*`）。

```text
$ ls CONTRIBUTING*   → 不存在
```

目前仅有 `README.md`、`AGENTS.md`、`PRD/` 描述项目用法与架构，但没有任何面向贡献者的约定文档。

**影响**

- 外部/协作者不知道分支策略、提交信息规范、测试要求、是否需要签名 DCO/CLA。
- 与第 1 项叠加：即便有人提 PR，也没有流程承接。

**建议**

- 新增 `CONTRIBUTING.md`，明确：开发环境搭建、构建与测试命令、代码风格（`gofmt`）、分支/PR 规范、Issue 提报方式。
- 注意：当前仓库 `.gitignore` 规则为 `*.md` 仅放行 `README.md` 与 `AGENTS.md`，新增的 `CONTRIBUTING.md` 需同步调整 `.gitignore` 的放行白名单，否则不会被纳入版本库。

---

## 3. ActivityTracker 需要清理

**现状**

`ActivityTracker` 在 `internal/agent/orchestration.go:26` 定义，是一个线程安全的子 Agent 活动记录器（`map[string]SubagentActivity` + `sync.Mutex`），被以下位置引用：

- `internal/agent/orchestration.go`（定义 + `SpawnSubagent` 使用，约 6 处）
- `internal/agent/agent.go`（Session 持有，约 2 处）
- `internal/agent/tools.go`（约 1 处）
- `internal/server/server.go`（约 2 处，向 Web UI 暴露活动数据）

核查发现的问题点：

1. **用户可见字符串硬编码为中文，绕过 i18n**：例如
   `orchestration.go` 中 `fmt.Errorf("子 Agent 委派深度已达到上限")`、`fmt.Errorf("Agent %q 不存在")`，均未走 `internal/i18n` 翻译层。
2. **可能存在冗余/未充分使用的路径**：该结构仅服务于子 Agent 活动追踪，但写入与消费链路分散在 agent 与 server 两侧，缺少单一职责边界，存在清理空间（死代码、未使用的字段/方法）。

**影响**

- 国际化不完整（见第 6 项），英文/繁体界面下会直接出现中文报错。
- 代码可读性/维护性下降，活动追踪与业务逻辑耦合。

**建议**

- 审计 `ActivityTracker` 的全部读写路径，移除未使用字段/方法。
- 将用户可见报错统一接入 `internal/i18n`。
- 明确其归属：若仅用于 Web UI 展示，考虑收敛到 server 层，避免 agent 核心循环承担展示态。

---

## 4. TUI 不支持鼠标 / 多窗格 / Markdown 渲染

**现状**

TUI 实现位于 `internal/ui/tui.go`，为**手写 ANSI 终端输出**（仅依赖 `github.com/mattn/go-isatty` + 自定义转义码），未使用 bubbletea / lipgloss / bubbles / tcell / glamour / rivo/tview 等任何终端 UI 框架。

```text
go.mod 中无 bubbletea / lipgloss / bubbles / tcell / glamour / tview 依赖
tui.go 中无 mouse / Mouse / pane / Pane / Split / markdown / glamour 引用
```

当前 TUI 仅提供基础的文本流与简单样式（bold/dim/italic ANSI 转义），缺失：

- **鼠标支持**：无法点击、滚动、选择。
- **多窗格布局**：没有分屏/面板（如对话区 + 文件树 + 活动面板并排）。
- **Markdown 渲染**：Agent 输出若含 Markdown，只能以原始文本展示，无语法高亮/排版。

**影响**

- 与 Web UI 能力差距明显，重度用户偏好终端时体验受限。
- 难以在不重写的前提下扩展交互（手写 ANSI 不适合做复杂布局）。

**建议**

- 短期：在文档/帮助中明确 TUI 的能力边界。
- 中期：若需增强，迁移到 bubbletea + lipgloss（布局）+ glamour（Markdown 渲染）+ 启用鼠标事件（`tea.MouseMotion`），而不是继续手写 ANSI。
- 评估是否值得投入：若 TUI 仅为辅助入口，可明确「富交互以 Web UI 为准」。

---

## 5. 没有 plug-in / extension 机制

**现状**

全仓搜索 `plugin` / `Plugin` / `extension` / `Extension` 无任何匹配（internal 范围）。项目存在技能系统（`internal/skills/**`、前端技能加载），但**不是**对外可扩展的插件/扩展机制：

- 没有插件注册表 / 生命周期钩子。
- 没有第三方扩展加载点（HTTP 中间件、工具注册、UI 扩展点）。
- MCP（`internal/mcp`）是外部协议接入，但不等同于用户态插件机制。

**影响**

- 社区/第三方无法在不改源码的前提下扩展工具、UI 或行为。
- 功能演进只能走「改核心代码」一条路，耦合度高。

**建议**

- 明确产品定位：是否需要插件机制？若需要，先定义最小扩展契约（如工具注册接口、配置驱动的加载）。
- 若短期不做，应在 README/AGENTS 中显式声明「暂不支持插件扩展」，避免预期错位。

---

## 6. i18n 仅覆盖中文（实际可用性）

**现状**

i18n 层存在但覆盖极不完整：

- **Go CLI 侧**：`internal/i18n/messages.go` 虽定义了 `zh` / `zh-TW` / `en` 三个 `Locale`，但目录 `catalog` 仅含 **6 个 MessageKey**（connected / disconnected / workspace_required / no_undo / undo_done / preview_path）。绝大多数用户可见文案（含 `ActivityTracker` 报错、agent 提示等）仍硬编码中文。
- **Web UI 侧**：`internal/server/web/index.html` 使用了 `data-i18n="..."` 属性（如 `appTitle`、`addProvider`、`changesSection` 等大量 key），但**仓库内不存在对应的翻译目录文件**（无 `en.json` / `zh-TW.json` 等），且 `<html lang="zh-CN">` 被写死。
- 综上：框架脚手架到位，但**真实交付物只有中文**。

**影响**

- 名义上支持多语言，实际上英文/繁体用户会看到中文硬编码串与未翻译的占位。
- 与第 3 项的硬编码中文报错叠加，国际化形同虚设。

**建议**

- 收敛范围：要么认真做（补全 Web UI 翻译目录 + 将所有硬编码串接入 i18n），要么明确「当前仅中文」，移除/弱化未实现的 locale 入口以避免误导。
- 优先把 `internal/i18n` 与 Web UI 的 `data-i18n` 打通到同一份目录源。

---

## 7. LongCat-frontend.exe 与 LongCat-frontend.exe~ 被提交进仓库（应忽略）

**现状**

- `LongCat-frontend.exe~` **当前已被 git 跟踪**：
  ```text
  $ git ls-files | grep -i '\.exe'
  LongCat-frontend.exe~
  ```
- `LongCat-frontend.exe` 本身被 `.gitignore` 的 `*.exe` 规则忽略（已验证 `git check-ignore` 命中 `.gitignore:7:*.exe`）。
- **问题根因**：`.gitignore` 的忽略模式为 `*.exe`，但编辑器/构建产生的备份文件 `LongCat-frontend.exe~`（后缀 `.exe~`）**不匹配** `*.exe`，因此漏网被提交。

```gitignore
# 当前规则
*.exe              # 仅忽略以 .exe 结尾
# 未覆盖 *.exe~ / *.exe.bak 等变体
```

**影响**

- 二进制产物进入版本库，膨胀仓库、污染 diff、且每次构建都会产生新的待提交噪声。
- 备份文件 `.exe~` 属于临时产物，无任何价值。

**建议**

- 立即从跟踪中移除（保留工作区文件）：
  ```bash
  git rm --cached LongCat-frontend.exe~
  ```
- 加固 `.gitignore`，覆盖所有变体：
  ```gitignore
  *.exe
  *.exe~
  *.exe.bak
  # 或统一：LongCat-frontend 二进制
  /LongCat-frontend
  /LongCat-frontend.exe*
  ```
- 提交该 `.gitignore` 修正，避免再次漏网。

---

## 汇总表

| # | 问题 | 状态 | 证据 |
|---|------|------|------|
| 1 | 缺少 CI/CD 配置 | 待处理 | 无 `.github`/`.gitlab-ci.yml`/`.circleci` |
| 2 | 缺少 CONTRIBUTING.md | 待处理 | `ls CONTRIBUTING*` 无结果 |
| 3 | ActivityTracker 需清理 | 待处理 | `orchestration.go:26`，硬编码中文报错 |
| 4 | TUI 无鼠标/多窗格/Markdown | 待处理 | `tui.go` 手写 ANSI，无框架依赖 |
| 5 | 无 plugin/extension 机制 | 待处理 | 全仓无 plugin/extension 匹配 |
| 6 | i18n 仅覆盖中文 | 待处理 | `messages.go` 仅 6 key；Web 无翻译目录 |
| 7 | exe/.exe~ 误提交 | 待处理 | `git ls-files` 含 `LongCat-frontend.exe~` |

---

_最后更新：2026-07-31_
