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
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/llm"
)

//go:embed web
var webFS embed.FS

type api struct {
	manager *llm.Manager
	session *agent.Session
	mu      sync.Mutex // 串行化对话，轻量会话无需并发
}

// Run 阻塞式启动 HTTP 服务。
func Run(addr string, m *llm.Manager, s *agent.Session) error {
	a := &api{manager: m, session: s}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", a.index)
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("POST /api/providers", a.addProvider)
	mux.HandleFunc("PUT /api/providers/{id}", a.updateProvider)
	mux.HandleFunc("DELETE /api/providers/{id}", a.removeProvider)
	mux.HandleFunc("POST /api/providers/{id}/use", a.useProvider)
	mux.HandleFunc("POST /api/chat", a.chat)
	mux.HandleFunc("POST /api/reset", a.reset)

	srv := &http.Server{Addr: addr, Handler: local(mux)}
	return srv.ListenAndServe()
}

// local 仅允许本机访问（Tauri WebView / 本地浏览器）。
func local(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.RemoteAddr
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}
		host = strings.Trim(host, "[]")
		if host != "127.0.0.1" && host != "::1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	writeJSON(w, 200, map[string]any{
		"providers": views,
		"skills":    skills,
		"protocols": protocols,
	})
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

// chat SSE 流式对话。
func (a *api) chat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message string `json:"message"`
		Mode    string `json:"mode,omitempty"` // 可选：前端模式 react|nextjs|vue|tailwind|svelte
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
		writeErr(w, 400, fmt.Errorf("message 不能为空"))
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

	if names := a.session.MatchedSkills(in.Message); len(names) > 0 {
		send("skills", names)
	}
	_, err := a.session.Ask(r.Context(), in.Message, func(delta string) {
		send("delta", delta)
	})
	if err != nil {
		send("error", err.Error())
		return
	}
	send("done", true)
}

func (a *api) reset(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	a.session.Reset()
	a.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
