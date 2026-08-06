// Package server 为 Tauri Desktop / 浏览器提供本地 HTTP API 与内嵌 Web UI。
//
// 端点:
//
//	GET    /                          Web UI（内嵌单文件应用）
//	GET    /api/state                 供应商列表 + 技能列表 + 当前激活项
//	POST   /api/providers             新增供应商
//	PUT    /api/providers/{id}        更新供应商（api_key 为空表示保留原值）
//	DELETE /api/providers/{id}        删除供应商
//	POST   /api/providers/{id}/use    切换当前供应商
//	POST   /api/chat                  对话（SSE 流式：event data 为文本增量）
//	POST   /api/reset                 重置会话
//	GET    /api/preview/state         预览历史状态
//	POST   /api/preview/navigate      记录预览导航
//	POST   /api/preview/back|forward  预览历史导航
//	GET    /api/workspace/events      工作区变更 SSE
//	GET    /api/workspace/diff        Agent 文件差异
//	GET/POST /api/workspace/undo      撤销历史与最近写入
//	GET/POST/DELETE /api/mcp          MCP 配置与健康信息
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/cache"
	"LongCat-frontend/internal/i18n"
	"LongCat-frontend/internal/im"
	"LongCat-frontend/internal/llm"
	"LongCat-frontend/internal/mcp"
	"LongCat-frontend/internal/memory"
	"LongCat-frontend/internal/skills"
	"LongCat-frontend/internal/workspace"
)

//go:embed web
var webFS embed.FS

type api struct {
	manager         *llm.Manager
	session         *agent.Session
	market          *skills.Market
	workspaces      *workspace.Store
	activeWorkspace workspace.Workspace
	activeSessionID string
	watcher         *workspace.Watcher
	undo            *workspace.UndoStore
	memory          *memory.Store
	mcp             *mcp.Manager
	im              *im.Bridge
	cache           *cache.Cache[string, any]
	locale          i18n.Locale
	settingsPath    string
	cancelMu        sync.Mutex
	cancel          context.CancelFunc
	mu              sync.Mutex // 串行化对话，轻量会话无需并发

	// IM 远程会话：与桌面端会话完全隔离，各 IM 会话独占一个 agent.Session。
	// 单独用 imMu 保护，避免被桌面端长对话持有的 mu 阻塞。
	imMu        sync.Mutex
	imSessions  map[string]*agent.Session
	imWorkspace atomic.Value // string，当前工作空间路径快照
}

// Run 阻塞式启动 HTTP 服务。
func Run(addr string, m *llm.Manager, s *agent.Session) error {
	mkt, err := skills.NewMarket()
	if err != nil {
		return fmt.Errorf("初始化 skills 市场失败: %w", err)
	}
	ws, err := workspace.NewStore()
	if err != nil {
		return fmt.Errorf("初始化工作空间存储失败: %w", err)
	}
	undo, err := workspace.NewUndoStore()
	if err != nil {
		return fmt.Errorf("初始化撤销存储失败: %w", err)
	}
	memStore, err := memory.NewStore()
	if err != nil {
		return fmt.Errorf("初始化记忆存储失败: %w", err)
	}
	mcpManager := mcp.NewManager("")
	_ = mcpManager.Load()
	imBridge, err := im.NewBridge()
	if err != nil {
		return fmt.Errorf("初始化 IM Bridge 失败: %w", err)
	}
	locale := i18n.DetectSystem()
	settingsPath := ""
	if dir, configErr := llm.ConfigDir(); configErr == nil {
		settingsPath = filepath.Join(dir, "settings.json")
		if pref, prefErr := i18n.LoadPreference(settingsPath); prefErr == nil && pref != i18n.PreferenceSystem {
			locale = i18n.Normalize(i18n.Locale(pref))
		}
	}
	s.MCP, s.Undo, s.Activity, s.Memory = mcpManager, undo, agent.NewActivityTracker(), memStore
	a := &api{manager: m, session: s, market: mkt, workspaces: ws, undo: undo, memory: memStore, mcp: mcpManager, im: imBridge, cache: cache.New[string, any](5*time.Minute, 1000), locale: locale, settingsPath: settingsPath}
	a.imWorkspace.Store(s.Workspace)
	// 注入 IM 消息处理器，并按上次持久化的启用意图恢复接收。
	if imBridge != nil {
		imBridge.SetHandler(a.handleIMMessage)
		if imBridge.Enabled() {
			imBridge.Start()
		}
	}
	mcpManager.StartHealthChecks(context.Background(), 30*time.Second)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", a.index)
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("POST /api/session/planmode", a.setPlanMode)
	mux.HandleFunc("POST /api/providers", a.addProvider)
	mux.HandleFunc("PUT /api/providers/{id}", a.updateProvider)
	mux.HandleFunc("DELETE /api/providers/{id}", a.removeProvider)
	mux.HandleFunc("POST /api/providers/{id}/use", a.useProvider)
	mux.HandleFunc("POST /api/chat", a.chat)
	mux.HandleFunc("POST /api/chat/stop", a.stopChat)
	mux.HandleFunc("POST /api/reset", a.reset)
	mux.HandleFunc("GET /api/skills/state", a.skillsState)
	mux.HandleFunc("POST /api/skills/repo/add", a.skillsRepoAdd)
	mux.HandleFunc("POST /api/skills/repo/remove", a.skillsRepoRemove)
	mux.HandleFunc("POST /api/skills/browse", a.skillsBrowse)
	mux.HandleFunc("POST /api/skills/install", a.skillsInstall)
	mux.HandleFunc("POST /api/skills/uninstall", a.skillsUninstall)
	mux.HandleFunc("POST /api/skills/use", a.skillsUse)
	mux.HandleFunc("GET /api/workspace", a.getWorkspace)
	mux.HandleFunc("GET /api/preview-file", a.previewFile)
	mux.HandleFunc("GET /api/preview/{path...}", a.previewFile)
	mux.HandleFunc("GET /api/preview/state", a.previewState)
	mux.HandleFunc("POST /api/preview/navigate", a.previewNavigate)
	mux.HandleFunc("POST /api/preview/back", a.previewBack)
	mux.HandleFunc("POST /api/preview/forward", a.previewForward)
	mux.HandleFunc("POST /api/workspace", a.setWorkspace)
	mux.HandleFunc("GET /api/workspaces", a.listWorkspaces)
	mux.HandleFunc("POST /api/workspaces/open", a.openWorkspace)
	mux.HandleFunc("GET /api/sessions", a.listSessions)
	mux.HandleFunc("POST /api/sessions", a.createSession)
	mux.HandleFunc("GET /api/sessions/{id}", a.getSession)
	mux.HandleFunc("PUT /api/sessions/{id}", a.updateSession)
	mux.HandleFunc("POST /api/sessions/{id}/use", a.useSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", a.deleteSession)
	mux.HandleFunc("GET /api/workspace/events", a.workspaceEvents)
	mux.HandleFunc("GET /api/workspace/diff", a.workspaceDiff)
	mux.HandleFunc("GET /api/workspace/undo", a.workspaceUndoList)
	mux.HandleFunc("POST /api/workspace/undo", a.workspaceUndo)
	mux.HandleFunc("GET /api/mcp", a.mcpState)
	mux.HandleFunc("POST /api/mcp", a.mcpUpsert)
	mux.HandleFunc("DELETE /api/mcp/{id}", a.mcpRemove)
	mux.HandleFunc("POST /api/mcp/refresh", a.mcpRefresh)
	mux.HandleFunc("GET /api/im/state", a.imState)
	mux.HandleFunc("POST /api/im/start", a.imStart)
	mux.HandleFunc("POST /api/im/stop", a.imStop)
	mux.HandleFunc("GET /api/im/instances", a.imInstances)
	mux.HandleFunc("POST /api/im/instances", a.imSaveInstance)
	mux.HandleFunc("DELETE /api/im/instances/{id}", a.imDeleteInstance)
	mux.HandleFunc("POST /api/im/scan/begin", a.imScanBegin)
	mux.HandleFunc("POST /api/im/scan/poll", a.imScanPoll)
	mux.HandleFunc("GET /api/im/doctor", a.imDoctor)
	mux.HandleFunc("POST "+im.WebhookPathPrefix+"{id}", a.imWebhook)
	mux.HandleFunc("GET /api/cache/stats", a.cacheStats)
	mux.HandleFunc("DELETE /api/cache/invalidate", a.cacheInvalidate)
	mux.HandleFunc("GET /api/settings/locale", a.getLocale)
	mux.HandleFunc("PUT /api/settings/locale", a.setLocale)

	// 记忆：CRUD + 云同步（git CLI，手动触发）
	mux.HandleFunc("GET /api/memory", a.memoryList)
	mux.HandleFunc("POST /api/memory", a.memoryCreate)
	mux.HandleFunc("GET /api/memory/read", a.memoryRead)
	mux.HandleFunc("PUT /api/memory", a.memoryUpdate)
	mux.HandleFunc("DELETE /api/memory", a.memoryDelete)
	mux.HandleFunc("GET /api/memory/sync/status", a.memorySyncStatus)
	mux.HandleFunc("POST /api/memory/sync/repo", a.memorySyncRepo)
	mux.HandleFunc("POST /api/memory/sync/push", a.memorySyncPush)
	mux.HandleFunc("POST /api/memory/sync/pull", a.memorySyncPull)

	srv := &http.Server{Addr: addr, Handler: local(mux)}
	return srv.ListenAndServe()
}

// local 仅允许本机访问（Tauri WebView / 本地浏览器）。
// 唯一例外是 IM 事件回调：开放平台必须能从外网推送事件，该路由自带
// 签名与 verification_token 校验，未配置校验凭据时仍然只接受本机请求。
func local(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, im.WebhookPathPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if !isLoopbackAddr(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackAddr 判断请求是否来自本机（含隧道客户端转发的场景）。
func isLoopbackAddr(remoteAddr string) bool {
	host := remoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func (a *api) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// previewFile serves a read-only file from the active workspace for the
// embedded browser. It deliberately reuses the same workspace-bound path
// rules as the agent tools and never accepts an arbitrary filesystem root.
func (a *api) previewFile(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	root := a.activeWorkspace.Path
	a.mu.Unlock()
	if root == "" {
		http.Error(w, "请先打开一个文件夹", http.StatusBadRequest)
		return
	}
	root, err := filepath.Abs(root)
	if err != nil {
		http.Error(w, "工作文件夹无效", http.StatusInternalServerError)
		return
	}
	resolvedRoot := root
	if realRoot, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
		resolvedRoot = filepath.Clean(realRoot)
	}
	requested := strings.TrimSpace(r.URL.Query().Get("path"))
	if requested == "" {
		requested = strings.TrimPrefix(r.PathValue("path"), "/")
	}
	if requested == "" {
		http.Error(w, "请输入预览文件路径", http.StatusBadRequest)
		return
	}
	var target string
	if filepath.IsAbs(requested) {
		target = filepath.Clean(requested)
	} else {
		target = filepath.Clean(filepath.Join(root, filepath.FromSlash(requested)))
	}
	if resolved, evalErr := filepath.EvalSymlinks(target); evalErr == nil {
		target = filepath.Clean(resolved)
	}
	rel, _ := filepath.Rel(resolvedRoot, target)
	if rel == "." || strings.HasSuffix(requested, "/") || strings.HasSuffix(requested, "\\") {
		target = filepath.Join(target, "index.html")
		if resolved, evalErr := filepath.EvalSymlinks(target); evalErr == nil {
			target = filepath.Clean(resolved)
		}
		rel, _ = filepath.Rel(resolvedRoot, target)
	}
	check, err := filepath.Rel(resolvedRoot, target)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		http.Error(w, "预览路径必须位于当前文件夹内", http.StatusForbidden)
		return
	}
	if resolved, evalErr := filepath.EvalSymlinks(target); evalErr == nil {
		r, relErr := filepath.Rel(resolvedRoot, resolved)
		if relErr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			http.Error(w, "预览路径不能通过符号链接离开当前文件夹", http.StatusForbidden)
			return
		}
	}
	file, err := os.Open(target)
	if err != nil {
		http.Error(w, "预览文件不存在: "+filepath.ToSlash(rel), http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "预览目标不是文件", http.StatusNotFound)
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(target))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline' 'unsafe-eval' data: blob: https:; img-src 'self' data: blob: https:;")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(target), info.ModTime().Round(time.Second), file)
}

type previewInput struct {
	URL string `json:"url"`
}

func (a *api) previewState(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeSessionID == "" {
		writeJSON(w, http.StatusOK, workspace.PreviewState{Index: -1})
		return
	}
	r, ok := a.workspaces.GetSession(a.activeSessionID)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("会话不存在"))
		return
	}
	writeJSON(w, http.StatusOK, r.Preview)
}

func (a *api) previewNavigate(w http.ResponseWriter, r *http.Request) {
	var in previewInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.URL) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("预览地址不能为空"))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	state, err := a.updatePreviewLocked(in.URL, true)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *api) previewBack(w http.ResponseWriter, _ *http.Request)    { a.movePreview(w, -1) }
func (a *api) previewForward(w http.ResponseWriter, _ *http.Request) { a.movePreview(w, 1) }

func (a *api) movePreview(w http.ResponseWriter, direction int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeSessionID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请先创建会话"))
		return
	}
	r, ok := a.workspaces.GetSession(a.activeSessionID)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("会话不存在"))
		return
	}
	next := r.Preview.Index + direction
	if next < 0 || next >= len(r.Preview.History) {
		writeJSON(w, http.StatusOK, r.Preview)
		return
	}
	r.Preview.Index = next
	r.Preview.CurrentURL = r.Preview.History[next]
	if err := a.workspaces.SaveSession(r); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, r.Preview)
}

func (a *api) updatePreviewLocked(rawURL string, persist bool) (workspace.PreviewState, error) {
	if a.activeSessionID == "" {
		return workspace.PreviewState{}, errors.New("请先创建会话")
	}
	r, ok := a.workspaces.GetSession(a.activeSessionID)
	if !ok {
		return workspace.PreviewState{}, errors.New("会话不存在")
	}
	value := strings.TrimSpace(rawURL)
	if value == "" {
		return r.Preview, errors.New("预览地址不能为空")
	}
	state := r.Preview
	if len(state.History) == 0 {
		state.History = []string{value}
		state.Index = 0
	} else if state.Index >= 0 && state.Index < len(state.History) && state.History[state.Index] == value { /* no-op */
	} else {
		if state.Index >= 0 && state.Index+1 < len(state.History) {
			state.History = append([]string(nil), state.History[:state.Index+1]...)
		}
		state.History = append(state.History, value)
		state.Index = len(state.History) - 1
		if len(state.History) > 100 {
			state.History = state.History[len(state.History)-100:]
			state.Index = len(state.History) - 1
		}
	}
	state.CurrentURL = value
	r.Preview = state
	if persist {
		if err := a.workspaces.SaveSession(r); err != nil {
			return state, err
		}
	}
	return state, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// providerView 是对外暴露的供应商视图（打码 key）。
type providerView struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	Priority int    `json:"priority"`
	Key      string `json:"key_redacted"`
	Active   bool   `json:"active"`
}

func (a *api) state(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	active := a.manager.ActiveID()
	views := []providerView{}
	for _, p := range a.manager.List() {
		views = append(views, providerView{
			ID: p.ID, URL: p.URL, Protocol: string(p.Protocol), Model: p.Model,
			Priority: p.Priority, Key: p.Redacted(), Active: p.ID == active,
		})
	}
	skills := []map[string]string{}
	for _, s := range a.session.Skills {
		skills = append(skills, map[string]string{"name": s.Title, "description": s.Description})
	}
	protocols := []string{}
	for _, p := range llm.SupportedProtocols() {
		protocols = append(protocols, string(p))
	}
	var ws any
	var sessions any = []any{}
	if a.activeWorkspace.ID != "" {
		ws = a.activeWorkspace
		sessions = a.workspaces.ListSessions(a.activeWorkspace.ID)
	}
	activity := []agent.SubagentActivity{}
	if a.session != nil && a.session.Activity != nil {
		activity = a.session.Activity.List()
	}
	writeJSON(w, 200, map[string]any{
		"providers":         views,
		"skills":            skills,
		"protocols":         protocols,
		"workspace":         ws,
		"workspaces":        a.workspaces.ListWorkspaces(),
		"sessions":          sessions,
		"active_session_id": a.activeSessionID,
		"mcp":               a.mcpStateValue(),
		"im":                a.imStateValue(),
		"agent_activity":    activity,
		"locale":            string(a.locale),
		"plan_mode":         a.session.PlanMode,
	})
}

func (a *api) mcpStateValue() []mcp.MCPServer {
	if a.mcp == nil {
		return []mcp.MCPServer{}
	}
	return a.mcp.List()
}

// setPlanMode 供桌面端 Web UI 切换规划/执行模式（POST /api/session/planmode，
// body: {"on": true|false}）。与 /plan、/execute 斜杠命令等价，但无对话噪声、可即时回显。
func (a *api) setPlanMode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, fmt.Errorf("无效的请求: %w", err))
		return
	}
	a.mu.Lock()
	a.session.SetPlanMode(in.On)
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "plan_mode": in.On})
}

func (a *api) mcpState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"servers": a.mcpStateValue()})
}
func (a *api) mcpRefresh(w http.ResponseWriter, r *http.Request) {
	if a.mcp == nil {
		writeJSON(w, http.StatusOK, map[string]any{"servers": []mcp.MCPServer{}})
		return
	}
	reports := a.mcp.Refresh(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"servers": a.mcp.List(), "reports": reports})
}

func (a *api) mcpUpsert(w http.ResponseWriter, r *http.Request) {
	if a.mcp == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("MCP 未启用"))
		return
	}
	var server mcp.MCPServer
	if err := json.NewDecoder(r.Body).Decode(&server); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.mcp.UpsertProjectServer(server); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *api) mcpRemove(w http.ResponseWriter, r *http.Request) {
	if a.mcp == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("MCP 未启用"))
		return
	}
	if err := a.mcp.RemoveProjectServer(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *api) imStateValue() im.BridgeStatus {
	if a.im == nil {
		return im.BridgeStatus{State: "unavailable", Backend: "none"}
	}
	return a.im.Status()
}
func (a *api) imState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"bridge": a.imStateValue(), "instances": a.imInstancesValue()})
}
func (a *api) imInstancesValue() []im.ChannelInstance {
	if a.im == nil {
		return []im.ChannelInstance{}
	}
	return a.im.ListInstances()
}
func (a *api) imStart(w http.ResponseWriter, _ *http.Request) {
	if a.im == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("IM Bridge 未启用"))
		return
	}
	writeJSON(w, http.StatusOK, a.im.Start())
}
func (a *api) imStop(w http.ResponseWriter, _ *http.Request) {
	if a.im == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("IM Bridge 未启用"))
		return
	}
	writeJSON(w, http.StatusOK, a.im.Stop())
}
func (a *api) imInstances(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"instances": a.imInstancesValue()})
}

func (a *api) imSaveInstance(w http.ResponseWriter, r *http.Request) {
	if a.im == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("IM Bridge 未启用"))
		return
	}
	var input struct {
		Instance         im.ChannelInstance `json:"instance"`
		Secrets          map[string]string  `json:"secrets"`
		ConnectAfterSave bool               `json:"connect_after_save"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	saved, err := a.im.SaveInstance(input.Instance, input.Secrets, input.ConnectAfterSave)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}
func (a *api) imDeleteInstance(w http.ResponseWriter, r *http.Request) {
	if a.im == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("IM Bridge 未启用"))
		return
	}
	if err := a.im.DeleteInstance(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *api) imScanBegin(w http.ResponseWriter, r *http.Request) {
	if a.im == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("IM Bridge 未启用"))
		return
	}
	var input struct {
		Channel string            `json:"channel"`
		Options map[string]string `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	result, err := a.im.ScanBegin(r.Context(), im.RemoteChannelID(input.Channel), input.Options)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (a *api) imScanPoll(w http.ResponseWriter, r *http.Request) {
	if a.im == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("IM Bridge 未启用"))
		return
	}
	var input struct {
		Channel    string `json:"channel"`
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	result, err := a.im.ScanPoll(r.Context(), im.RemoteChannelID(input.Channel), input.DeviceCode)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (a *api) imDoctor(w http.ResponseWriter, _ *http.Request) {
	if a.im == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "instances": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, a.im.Doctor())
}

func (a *api) cacheStats(w http.ResponseWriter, _ *http.Request) {
	if a.cache == nil {
		writeJSON(w, http.StatusOK, cache.Stats{})
		return
	}
	writeJSON(w, http.StatusOK, a.cache.Stats())
}
func (a *api) cacheInvalidate(w http.ResponseWriter, r *http.Request) {
	if a.cache != nil {
		key := r.URL.Query().Get("key")
		if key == "" {
			a.cache.Clear()
		} else {
			a.cache.Delete(key)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *api) getLocale(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"locale": string(a.locale)})
}
func (a *api) setLocale(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Locale string `json:"locale"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	preference := i18n.Preference(in.Locale)
	locale := i18n.DetectSystem()
	if preference != i18n.PreferenceSystem {
		locale = i18n.Normalize(i18n.Locale(preference))
	}
	if a.settingsPath != "" {
		if err := i18n.SavePreference(a.settingsPath, preference); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	a.mu.Lock()
	a.locale = locale
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"locale": string(locale), "preference": string(preference)})
}

type providerInput struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	APIKey   string `json:"api_key"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	Priority int    `json:"priority"`
}

func (a *api) addProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	err := a.manager.Add(llm.Provider{
		ID: in.ID, URL: in.URL, APIKey: in.APIKey,
		Protocol: llm.Protocol(in.Protocol), Model: in.Model, Priority: in.Priority,
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *api) updateProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in providerInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	old, ok := a.manager.Get(id)
	if !ok {
		writeErr(w, 404, fmt.Errorf("供应商 %q 不存在", id))
		return
	}
	key := in.APIKey
	if key == "" {
		key = old.APIKey // 空 key 表示保留原值
	}
	err := a.manager.Update(llm.Provider{
		ID: id, URL: in.URL, APIKey: key,
		Protocol: llm.Protocol(in.Protocol), Model: in.Model, Priority: in.Priority,
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *api) removeProvider(w http.ResponseWriter, r *http.Request) {
	if err := a.manager.Remove(r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *api) useProvider(w http.ResponseWriter, r *http.Request) {
	if err := a.manager.SetActive(r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// planExecuteSlash 解析规划/执行模式切换命令，返回模式 token 与是否匹配。
// 仅识别 /plan、/execute、/mode plan、/mode execute；其它 /mode 子命令（框架模式）
// 或 /技能名 不在此处理。
func planExecuteSlash(msg string) (string, bool) {
	m := strings.TrimSpace(msg)
	switch m {
	case "/plan", "/execute":
		return strings.TrimPrefix(m, "/"), true
	}
	if strings.HasPrefix(m, "/mode") {
		rest := strings.TrimSpace(strings.TrimPrefix(m, "/mode"))
		switch rest {
		case "plan":
			return "plan", true
		case "execute":
			return "execute", true
		}
	}
	return "", false
}

// chat SSE 流式对话。
func (a *api) chat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message     string           `json:"message"`
		Mode        string           `json:"mode,omitempty"` // 可选：前端模式 react|nextjs|vue|tailwind|svelte
		Attachments []llm.Attachment `json:"attachments,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, fmt.Errorf("无效的聊天请求: %w", err))
		return
	}
	if err := validateAttachments(in.Attachments); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(in.Message) == "" && len(in.Attachments) == 0 {
		writeErr(w, 400, fmt.Errorf("message 或附件不能为空"))
		return
	}
	// 处理规划/执行模式切换命令（/plan、/execute、/mode plan、/mode execute）。
	// 这些命令不送入模型，直接响应后返回。
	if cmd, ok := planExecuteSlash(in.Message); ok {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeErr(w, 500, fmt.Errorf("streaming unsupported"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		send := func(event string, payload any) {
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			flusher.Flush()
		}
		switch cmd {
		case "plan":
			a.session.SetPlanMode(true)
			send("message", "✔ 已切换到 Plan 规划模式：只规划、可创建文档，不修改代码。用 /execute 恢复执行。")
		case "execute":
			a.session.SetPlanMode(false)
			send("message", "✔ 已切换到 Execute 执行模式：可正常执行代码改动。")
		}
		send("done", true)
		return
	}
	// 设置模式（空则保持当前模式；非法模式忽略）
	if in.Mode != "" {
		_ = a.session.SetMode(in.Mode)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	send := func(event string, payload any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeWorkspace.ID == "" || a.activeSessionID == "" {
		send("error", "请先打开一个文件夹并创建会话")
		return
	}

	// 检测 /技能名 命令，直接激活该技能
	msg := strings.TrimSpace(in.Message)
	if strings.HasPrefix(msg, "/") && !strings.Contains(msg, " ") {
		skillName := strings.TrimPrefix(msg, "/")
		// 检查技能是否存在
		installed := a.market.ListInstalled()
		found := false
		for _, name := range installed {
			if name == skillName {
				found = true
				break
			}
		}
		if found {
			a.session.ActiveSkill = skillName
			send("skills", []string{skillName})
		}
	}

	if names := a.session.MatchedSkills(in.Message); len(names) > 0 {
		send("skills", names)
	}
	ctx, cancel := context.WithCancel(r.Context())
	a.cancelMu.Lock()
	a.cancel = cancel
	a.cancelMu.Unlock()
	defer func() {
		cancel()
		a.cancelMu.Lock()
		a.cancel = nil
		a.cancelMu.Unlock()
	}()
	// 消费 agent 的 goroutine+channel 流式事件，逐条 flush 为 SSE 帧。
	evCh, err := a.session.Stream(ctx, in.Message, in.Attachments)
	if err != nil {
		send("error", err.Error())
		return
	}
	for ev := range evCh {
		switch ev.Kind {
		case "delta":
			send("delta", ev.Delta)
		case "tool":
			send("tool", ev.Tool)
		case "done":
			if ev.Err != nil {
				if errors.Is(ev.Err, context.Canceled) {
					_ = a.saveActiveLocked()
					send("stopped", true)
					send("done", true)
					return
				}
				send("error", ev.Err.Error())
				return
			}
			if err := a.saveActiveLocked(); err != nil {
				send("error", err.Error())
				return
			}
			send("done", true)
			return
		}
	}
}

func validateAttachments(attachments []llm.Attachment) error {
	if len(attachments) > 10 {
		return fmt.Errorf("一次最多上传 10 个文件")
	}
	var total int64
	for _, a := range attachments {
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("附件缺少文件名")
		}
		if strings.TrimSpace(a.MIMEType) == "" {
			return fmt.Errorf("附件 %q 缺少 MIME 类型", a.Name)
		}
		bytes := int64(len(a.Data) + len(a.Text))
		if a.Size > 15<<20 || bytes > 22<<20 {
			return fmt.Errorf("附件 %q 超过 15 MB 限制", a.Name)
		}
		total += bytes
	}
	if total > 48<<20 {
		return fmt.Errorf("附件总大小不能超过 48 MB")
	}
	return nil
}

func (a *api) stopChat(w http.ResponseWriter, _ *http.Request) {
	a.cancelMu.Lock()
	cancel := a.cancel
	a.cancelMu.Unlock()
	if cancel == nil {
		writeJSON(w, 200, map[string]any{"ok": true, "stopped": false})
		return
	}
	cancel()
	writeJSON(w, 200, map[string]any{"ok": true, "stopped": true})
}

func (a *api) reset(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	a.session.Reset()
	_ = a.saveActiveLocked()
	a.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ==================== skills 市场 ====================

func (a *api) skillsState(w http.ResponseWriter, _ *http.Request) {
	repos, _ := a.market.ListRepos()
	installed := a.market.ListInstalled()
	writeJSON(w, 200, map[string]any{
		"repos":     repos,
		"installed": installed,
		"active":    a.session.ActiveSkill,
	})
}

func (a *api) skillsRepoAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.URL) == "" {
		writeErr(w, 400, fmt.Errorf("url 不能为空"))
		return
	}
	if err := a.market.AddRepo(in.URL); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *api) skillsRepoRemove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if err := a.market.RemoveRepo(in.URL); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *api) skillsBrowse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	list, err := a.market.BrowseRepo(in.URL)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	repo, _ := a.market.GetRepo(in.URL)
	writeJSON(w, 200, map[string]any{"repo": repo, "skills": list})
}

func (a *api) skillsInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if err := a.market.Install(in.URL, in.Name); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.session.ReloadSkills()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *api) skillsUninstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if err := a.market.Uninstall(in.Name); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.session.ReloadSkills()
	if a.session.ActiveSkill == in.Name {
		a.session.ActiveSkill = ""
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *api) skillsUse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	a.session.ActiveSkill = in.Name
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ==================== workspace 工作空间 ====================

func (a *api) getWorkspace(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeWorkspace.ID == "" {
		writeJSON(w, 200, map[string]any{
			"workspace":         "",
			"workspaces":        a.workspaces.ListWorkspaces(),
			"sessions":          []any{},
			"active_session_id": "",
			"messages":          []llm.Message{},
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"workspace":         a.activeWorkspace.Path,
		"workspace_info":    a.activeWorkspace,
		"workspaces":        a.workspaces.ListWorkspaces(),
		"sessions":          a.workspaces.ListSessions(a.activeWorkspace.ID),
		"active_session_id": a.activeSessionID,
		"messages":          append([]llm.Message{}, a.session.Messages...),
		"preview":           a.previewForActiveLocked(),
	})
}

func (a *api) previewForActiveLocked() workspace.PreviewState {
	if a.activeSessionID == "" {
		return workspace.PreviewState{Index: -1}
	}
	if r, ok := a.workspaces.GetSession(a.activeSessionID); ok {
		return r.Preview
	}
	return workspace.PreviewState{Index: -1}
}

func (a *api) workspaceEvents(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	watcher := a.watcher
	a.mu.Unlock()
	if watcher == nil {
		writeErr(w, http.StatusBadRequest, errors.New("请先打开一个工作空间"))
		return
	}
	ch, unsubscribe := watcher.Subscribe()
	defer unsubscribe()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	for {
		select {
		case event, open := <-ch:
			if !open {
				return
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: change\ndata: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *api) workspaceDiff(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	root := a.activeWorkspace.Path
	undo := a.undo
	a.mu.Unlock()
	if root == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请先打开一个工作空间"))
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeErr(w, http.StatusBadRequest, errors.New("path 不能为空"))
		return
	}
	diff, err := undo.Diff(root, path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

func (a *api) workspaceUndoList(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	root := a.activeWorkspace.Path
	undo := a.undo
	a.mu.Unlock()
	if root == "" {
		writeJSON(w, http.StatusOK, []workspace.UndoEntry{})
		return
	}
	writeJSON(w, http.StatusOK, undo.List(root))
}
func (a *api) workspaceUndo(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	root := a.activeWorkspace.Path
	undo := a.undo
	a.mu.Unlock()
	if root == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请先打开一个工作空间"))
		return
	}
	entry, err := undo.UndoLast(root)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entry": entry})
}

func (a *api) setWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, fmt.Errorf("无效的请求"))
		return
	}
	if err := a.openFolder(strings.TrimSpace(in.Workspace)); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "workspace": a.activeWorkspace, "session_id": a.activeSessionID})
}

func (a *api) listWorkspaces(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"workspaces": a.workspaces.ListWorkspaces(), "active_workspace_id": a.activeWorkspace.ID})
}

func (a *api) openWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path      string `json:"path"`
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, fmt.Errorf("无效的请求"))
		return
	}
	path := in.Path
	if strings.TrimSpace(path) == "" {
		path = in.Workspace
	}
	if err := a.openFolder(path); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"workspace": a.activeWorkspace, "sessions": a.workspaces.ListSessions(a.activeWorkspace.ID), "active_session_id": a.activeSessionID})
}

func (a *api) openFolder(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, err := a.workspaces.OpenFolder(path)
	if err != nil {
		return err
	}
	if err := a.saveActiveLocked(); err != nil {
		return err
	}
	a.activeWorkspace = w
	sessions := a.workspaces.ListSessions(w.ID)
	if len(sessions) == 0 {
		r, e := a.workspaces.CreateSession(w.ID, "新会话")
		if e != nil {
			return e
		}
		sessions = []workspace.SessionRecord{r}
	}
	if err := a.activateSessionLocked(sessions[0].ID); err != nil {
		return err
	}
	return a.configureWorkspaceLocked(w.Path)
}

func (a *api) configureWorkspaceLocked(root string) error {
	if a.watcher == nil || !workspaceSameRoot(a.watcher.Root(), root) {
		if a.watcher != nil {
			a.watcher.Stop()
		}
		watcher, err := workspace.NewWatcher(root, 500*time.Millisecond)
		if err != nil {
			return err
		}
		a.watcher = watcher
		watcher.Start(context.Background())
	}
	if a.mcp != nil {
		_ = a.mcp.SetWorkspace(root)
	}
	if dir, err := llm.ConfigDir(); err == nil {
		if discovered, discoverErr := agent.DiscoverAgents(root, filepath.Join(dir, "agents"), ""); discoverErr == nil {
			a.session.Agents = discovered
		}
	}
	return nil
}

func workspaceSameRoot(aPath, bPath string) bool {
	aa, _ := filepath.Abs(aPath)
	bb, _ := filepath.Abs(bPath)
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

func (a *api) listSessions(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeWorkspace.ID == "" {
		writeJSON(w, 200, map[string]any{"sessions": []any{}, "active_session_id": ""})
		return
	}
	writeJSON(w, 200, map[string]any{"workspace": a.activeWorkspace, "sessions": a.workspaces.ListSessions(a.activeWorkspace.ID), "active_session_id": a.activeSessionID})
}

func (a *api) createSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeWorkspace.ID == "" {
		writeErr(w, 400, fmt.Errorf("请先打开一个文件夹"))
		return
	}
	if err := a.saveActiveLocked(); err != nil {
		writeErr(w, 500, err)
		return
	}
	rc, err := a.workspaces.CreateSession(a.activeWorkspace.ID, in.Title)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.activateSessionLocked(rc.ID); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"session": rc, "active_session_id": a.activeSessionID})
}

func (a *api) getSession(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rc, ok := a.workspaces.GetSession(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, fmt.Errorf("会话不存在"))
		return
	}
	writeJSON(w, 200, map[string]any{"session": rc, "active": rc.ID == a.activeSessionID})
}

func (a *api) updateSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, fmt.Errorf("无效的请求"))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rc, ok := a.workspaces.GetSession(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, fmt.Errorf("会话不存在"))
		return
	}
	updated, err := a.workspaces.UpdateSessionTitle(rc.ID, in.Title)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"session": updated, "active_session_id": a.activeSessionID})
}

func (a *api) useSession(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.activateSessionLocked(r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "active_session_id": a.activeSessionID})
}

func (a *api) deleteSession(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := r.PathValue("id")
	if id == a.activeSessionID {
		sessions := a.workspaces.ListSessions(a.activeWorkspace.ID)
		if len(sessions) <= 1 {
			writeErr(w, 400, fmt.Errorf("工作空间至少需要保留一个会话"))
			return
		}
		if err := a.saveActiveLocked(); err != nil {
			writeErr(w, 500, err)
			return
		}
		for _, candidate := range sessions {
			if candidate.ID != id {
				if err := a.activateSessionLocked(candidate.ID); err != nil {
					writeErr(w, 500, err)
					return
				}
				break
			}
		}
	}
	if err := a.workspaces.DeleteSession(id); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "active_session_id": a.activeSessionID})
}

func (a *api) saveActiveLocked() error {
	if a.activeSessionID == "" {
		return nil
	}
	r, ok := a.workspaces.GetSession(a.activeSessionID)
	if !ok {
		return nil
	}
	r.Messages = append([]llm.Message(nil), a.session.Messages...)
	r.Mode = a.session.Mode
	r.ActiveSkill = a.session.ActiveSkill
	r.Title = sessionTitle(r.Title, r.Messages)
	return a.workspaces.SaveSession(r)
}

func sessionTitle(current string, messages []llm.Message) string {
	if title := strings.TrimSpace(current); title != "" && title != "新会话" {
		return title
	}
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		title := strings.Join(strings.Fields(message.Content), " ")
		if title == "" && len(message.Attachments) > 0 {
			names := make([]string, 0, len(message.Attachments))
			for _, attachment := range message.Attachments {
				if strings.TrimSpace(attachment.Name) != "" {
					names = append(names, attachment.Name)
				}
			}
			title = "附件：" + strings.Join(names, "、")
		}
		if title == "" {
			continue
		}
		runes := []rune(title)
		if len(runes) > 36 {
			return string(runes[:36]) + "…"
		}
		return title
	}
	return "新会话"
}

func (a *api) activateSessionLocked(id string) error {
	r, ok := a.workspaces.GetSession(id)
	if !ok {
		return fmt.Errorf("会话 %q 不存在", id)
	}
	w, ok := a.workspaces.GetWorkspace(r.WorkspaceID)
	if !ok {
		return errors.New("会话所属工作空间不存在")
	}
	a.activeWorkspace = w
	a.activeSessionID = id
	a.session.Workspace = w.Path
	a.imWorkspace.Store(w.Path)
	a.session.Messages = append([]llm.Message(nil), r.Messages...)
	a.session.Mode = r.Mode
	a.session.ActiveSkill = r.ActiveSkill
	a.session.Undo = a.undo
	a.session.MCP = a.mcp
	if a.session.Activity == nil {
		a.session.Activity = agent.NewActivityTracker()
	}
	return nil
}
