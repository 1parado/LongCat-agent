package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/im"
)

// imTurnTimeout 单条远程消息允许的最长处理时间。
const imTurnTimeout = 5 * time.Minute

// imWebhook 接收开放平台推送的事件回调。每个 IM 实例有独立的回调地址
// （/api/im/webhook/{id}），鉴权与解密全部由 im.Bridge 负责。
func (a *api) imWebhook(w http.ResponseWriter, r *http.Request) {
	if a.im == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("IM Bridge 未启用"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("读取回调报文失败: %w", err))
		return
	}
	result, err := a.im.HandleWebhook(r.Context(), r.PathValue("id"), r.Header, body, isLoopbackAddr(r.RemoteAddr))
	if err != nil {
		status := result.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		writeErr(w, status, err)
		return
	}
	status := result.Status
	if status == 0 {
		status = http.StatusOK
	}
	writeJSON(w, status, result.Body)
}

// handleIMMessage 是注入给 im.Bridge 的消息处理器。Bridge 用单条
// dispatcher goroutine 串行调用它，因此这里不需要再额外加锁保护会话。
func (a *api) handleIMMessage(ctx context.Context, msg im.IncomingMessage) (string, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return "", nil
	}
	switch cmd := im.ParseControl(text); cmd.Type {
	case im.ControlHelp:
		return imHelpText(), nil
	case im.ControlWhoami:
		return a.imWhoami(msg), nil
	case im.ControlNewSession:
		a.resetIMSession(msg)
		if q := strings.TrimSpace(cmd.Query); q != "" {
			text = q
			break
		}
		return "✔ 已开启新会话，历史已清空。", nil
	case im.ControlStop, im.ControlCancel:
		a.resetIMSession(msg)
		return "✔ 已终止当前会话上下文。", nil
	case im.ControlStatus, im.ControlProjectList, im.ControlProjectSelect, im.ControlResumeList, im.ControlResumeSelect:
		return "该命令暂未在远程会话中开放，请在桌面端操作。发送 /help 查看可用命令。", nil
	}

	session, err := a.imSessionFor(msg)
	if err != nil {
		return "", err
	}
	turnCtx, cancel := context.WithTimeout(ctx, imTurnTimeout)
	defer cancel()
	reply, err := session.Ask(turnCtx, im.VisibleUserContent(msg.Channel, text), nil)
	if err != nil {
		if reply != "" {
			return reply + "\n\n⚠️ 本轮未正常结束：" + err.Error(), nil
		}
		return "", err
	}
	if strings.TrimSpace(reply) == "" {
		return "（本轮没有产生文本输出）", nil
	}
	return reply, nil
}

// imSessionKey 用渠道 + 会话 ID 做键，保证不同群/私聊各自独立上下文。
func imSessionKey(msg im.IncomingMessage) string {
	return string(msg.Channel) + ":" + msg.ChatID
}

// imSessionFor 懒加载该 IM 会话专属的 agent.Session。它不复用桌面端会话，
// 避免远程消息污染本地对话历史，也避免与桌面端争抢同一份 Messages。
func (a *api) imSessionFor(msg im.IncomingMessage) (*agent.Session, error) {
	workspace, _ := a.imWorkspace.Load().(string)
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("尚未打开工作文件夹，请先在桌面端打开一个项目")
	}
	key := imSessionKey(msg)
	a.imMu.Lock()
	defer a.imMu.Unlock()
	if a.imSessions == nil {
		a.imSessions = map[string]*agent.Session{}
	}
	if session, ok := a.imSessions[key]; ok {
		if session.Workspace != workspace {
			session.Workspace = workspace
			session.Reset()
		}
		return session, nil
	}
	session, err := agent.NewSession(a.manager, "")
	if err != nil {
		return nil, fmt.Errorf("创建远程会话失败: %w", err)
	}
	session.Workspace = workspace
	session.MCP, session.Undo = a.mcp, a.undo
	session.Activity = agent.NewActivityTracker()
	a.imSessions[key] = session
	return session, nil
}

func (a *api) resetIMSession(msg im.IncomingMessage) {
	a.imMu.Lock()
	defer a.imMu.Unlock()
	delete(a.imSessions, imSessionKey(msg))
}

func (a *api) imWhoami(msg im.IncomingMessage) string {
	workspace, _ := a.imWorkspace.Load().(string)
	if workspace == "" {
		workspace = "（未打开）"
	}
	model := "（未配置）"
	if p, err := a.manager.Active(); err == nil {
		model = p.Model
	}
	return fmt.Sprintf("渠道: %s\n会话: %s\n用户: %s\n工作空间: %s\n模型: %s",
		msg.Channel, msg.ChatID, msg.UserID, workspace, model)
}

func imHelpText() string {
	return strings.Join([]string{
		"可用命令：",
		"/new  开启新会话（清空上下文）",
		"/stop 终止当前会话上下文",
		"/whoami 查看当前渠道、工作空间与模型",
		"/help 显示本帮助",
		"",
		"直接发送文本即可让 Agent 处理。",
	}, "\n")
}
