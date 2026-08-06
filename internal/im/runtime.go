package im

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// WebhookPathPrefix 是事件订阅回调的路由前缀，实例 ID 拼在后面构成每个
// 机器人专属的回调地址。server 与 Web UI 共用这个常量，避免两边写死不一致。
const WebhookPathPrefix = "/api/im/webhook/"

// TransportWebhook 是可选的入站传输方式：开放平台事件订阅（HTTP 回调），
// 需要把 WebhookPath 填到开放平台并配置公网可达地址。
const TransportWebhook = "webhook"

// TransportLongConn 是默认入站传输方式：飞书/Lark WebSocket 长连接，
// 由开放平台直接把事件推送到本地连接，无需公网回调地址/encrypt_key。
const TransportLongConn = "longconn"

// MessageHandler 处理一条入站消息并返回要回复的文本。
// 由上层（HTTP server）注入，Bridge 只负责传输、鉴权、过滤与回投。
type MessageHandler func(ctx context.Context, msg IncomingMessage) (string, error)

const (
	inboxSize       = 64
	dedupeTTL       = 5 * time.Minute
	enqueueTimeout  = 3 * time.Second
	shutdownTimeout = 2 * time.Second
)

// SetHandler 注入消息处理器。必须在 Start 之前调用，通常在服务启动时完成。
func (b *Bridge) SetHandler(h MessageHandler) {
	b.mu.Lock()
	b.handler = h
	b.mu.Unlock()
}

// Enabled 返回持久化的启用意图，用于进程启动时自动恢复接收。
func (b *Bridge) Enabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled
}

// Start 打开 IM 接收：为每个已启用且有凭据的实例挂载传输客户端，并启动
// 单条 dispatcher goroutine 串行消费入站队列。重复调用是幂等的。
func (b *Bridge) Start() BridgeStatus {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return b.Status()
	}
	b.enabled, b.lastError = true, ""
	ctx, cancel := context.WithCancel(context.Background())
	b.runCancel = cancel
	b.inbox = make(chan IncomingMessage, inboxSize)
	b.transports = map[string]*feishuClient{}
	b.longConns = map[string]*feishuLongConn{}
	b.seen = map[string]time.Time{}

	ready := 0
	for id, instance := range b.instances {
		if !instance.Enabled || len(b.secrets[id]) == 0 {
			continue
		}
		switch instance.Channel {
		case ChannelFeishu, ChannelLark:
			secret := b.secrets[id]
			appID, appSecret := secret["app_id"], secret["app_secret"]
			if appID == "" || appSecret == "" {
				instance.LastError = "缺少 app_id / app_secret，请重新扫码接入"
				b.instances[id] = instance
				continue
			}
			client := newFeishuClient(instance.Channel, appID, appSecret, b.client)
			if b.openBaseOverride != "" {
				client.base = b.openBaseOverride
			}
			b.transports[id] = client
			// 长连接收消息：扫码时已通过 Addons 自动订阅 im.message.receive_v1，
			// 这里建立 WebSocket 长连接即可接收，无需公网回调地址。
			lc, lcErr := newFeishuLongConn(id, instance.Channel, appID, appSecret, b.Deliver)
			if lcErr != nil {
				instance.LastError = "长连接建立失败: " + lcErr.Error()
				b.instances[id] = instance
				continue
			}
			b.longConns[id] = lc
			if !b.disableLongConn {
				go lc.run(ctx)
			}
			instance.LastError = ""
			b.instances[id] = instance
			ready++
		default:
			instance.LastError = "该渠道暂不支持接收消息（当前仅支持飞书 / Lark 长连接接收）"
			b.instances[id] = instance
		}
	}
	if ready == 0 {
		b.lastError = "没有可接收消息的实例：请先扫码接入飞书 / Lark，并确认实例处于启用状态"
	}
	b.running = true
	inbox := b.inbox
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.dispatch(ctx, inbox)
	}()
	_ = b.saveLocked()
	b.mu.Unlock()

	go b.verifyTransports(ctx)
	return b.Status()
}

// Stop 关闭 IM 接收：取消 dispatcher 上下文并卸载所有传输客户端。
// 生产者（webhook 处理器）在 ctx 取消后会立即拿到错误，不会阻塞。
func (b *Bridge) Stop() BridgeStatus {
	b.mu.Lock()
	cancel := b.runCancel
	b.runCancel, b.running, b.enabled = nil, false, false
	b.inbox = nil
	for id, lc := range b.longConns {
		lc.Close()
		delete(b.longConns, id)
	}
	b.transports = map[string]*feishuClient{}
	for id, instance := range b.instances {
		instance.LastError = ""
		b.instances[id] = instance
	}
	_ = b.saveLocked()
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		// 正在处理的那一轮对话可能还没返回，让它自行随 ctx 结束即可。
	}
	return b.Status()
}

// verifyTransports 用真实的 tenant_access_token 请求验证凭据是否可用，
// 结果写回实例状态，让“已配置”与“真正能收发”区分开。
func (b *Bridge) verifyTransports(ctx context.Context) {
	b.mu.Lock()
	targets := make(map[string]*feishuClient, len(b.transports))
	for id, client := range b.transports {
		targets[id] = client
	}
	b.mu.Unlock()
	for id, client := range targets {
		if ctx.Err() != nil {
			return
		}
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := client.Verify(reqCtx)
		cancel()
		b.mu.Lock()
		if instance, ok := b.instances[id]; ok {
			if err != nil {
				instance.LastError = err.Error()
				instance.Status = StatusError
			} else {
				instance.LastError = ""
				instance.Status = StatusConnected
			}
			b.instances[id] = instance
		}
		b.mu.Unlock()
	}
}

// dispatch 串行消费入站队列。单 goroutine 保证同一时刻只有一轮 Agent 在跑，
// 也让 agent.Session 的非并发安全字段无需加锁。ctx 取消即退出，无泄漏。
func (b *Bridge) dispatch(ctx context.Context, inbox <-chan IncomingMessage) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-inbox:
			if !ok {
				return
			}
			b.handleMessage(ctx, msg)
		}
	}
}

// Deliver 把一条入站消息投递到队列。接收未启用时直接报错；队列积压时超时
// 返回，绝不让调用方（HTTP 处理器）无限阻塞。
func (b *Bridge) Deliver(ctx context.Context, msg IncomingMessage) error {
	b.mu.Lock()
	inbox, running := b.inbox, b.running
	b.mu.Unlock()
	if !running || inbox == nil {
		return errors.New("IM 接收未启用，请先在 IM 面板点击“启用接收”")
	}
	select {
	case inbox <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(enqueueTimeout):
		return errors.New("IM 消息队列繁忙，请稍后重试")
	}
}

func (b *Bridge) handleMessage(ctx context.Context, msg IncomingMessage) {
	b.mu.Lock()
	instance, ok := b.instances[msg.InstanceID]
	handler := b.handler
	transport := b.transports[msg.InstanceID]
	b.mu.Unlock()
	if !ok || handler == nil || transport == nil {
		return
	}
	if !aclAllows(instance.ACL, msg) {
		return
	}
	reply, err := handler(ctx, msg)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		reply = "⚠️ 处理失败：" + err.Error()
	}
	if strings.TrimSpace(reply) == "" {
		return
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if sendErr := transport.ReplyText(sendCtx, msg.MessageID, msg.ChatID, reply); sendErr != nil {
		b.mu.Lock()
		if current, exists := b.instances[msg.InstanceID]; exists {
			current.LastError = "回复失败: " + sendErr.Error()
			b.instances[msg.InstanceID] = current
		}
		b.mu.Unlock()
	}
}

// aclAllows 应用实例级访问控制。默认策略是放行，只在显式配置时收紧。
func aclAllows(acl ACLConfig, msg IncomingMessage) bool {
	isGroup := msg.ChatType == "group"
	if acl.GroupOnly && !isGroup {
		return false
	}
	if acl.RequireMention && isGroup && !msg.Mentioned {
		return false
	}
	if list := splitACLList(acl.AllowFrom); len(list) > 0 && !containsFold(list, msg.UserID) {
		return false
	}
	if list := splitACLList(acl.AllowChat); len(list) > 0 && !containsFold(list, msg.ChatID) {
		return false
	}
	return true
}

func splitACLList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == ' '
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func containsFold(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(item, value) {
			return true
		}
	}
	return false
}

// ---------- 事件订阅回调 ----------

// WebhookResult 是 webhook 处理结果，由 HTTP 层原样写回给开放平台。
type WebhookResult struct {
	Status int
	Body   map[string]any
}

// HandleWebhook 处理一次开放平台事件回调。trusted 表示请求来自本机
// （例如 cloudflared / ngrok 隧道转发），未配置任何校验凭据时只接受本机请求。
func (b *Bridge) HandleWebhook(ctx context.Context, instanceID string, header http.Header, body []byte, trusted bool) (WebhookResult, error) {
	b.mu.Lock()
	instance, ok := b.instances[instanceID]
	secret := map[string]string{}
	for k, v := range b.secrets[instanceID] {
		secret[k] = v
	}
	running := b.running
	b.mu.Unlock()
	if !ok {
		return WebhookResult{Status: http.StatusNotFound}, errors.New("IM 实例不存在")
	}
	if instance.Channel != ChannelFeishu && instance.Channel != ChannelLark {
		return WebhookResult{Status: http.StatusBadRequest}, fmt.Errorf("渠道 %s 暂不支持事件订阅", instance.Channel)
	}
	encryptKey, verifyToken := secret["encrypt_key"], secret["verification_token"]
	if !trusted && encryptKey == "" && verifyToken == "" {
		return WebhookResult{Status: http.StatusForbidden}, errors.New("外部回调必须配置 encrypt_key 或 verification_token")
	}

	var envelope feishuEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return WebhookResult{Status: http.StatusBadRequest}, fmt.Errorf("回调报文解析失败: %w", err)
	}
	if envelope.Encrypt != "" {
		if encryptKey == "" {
			return WebhookResult{Status: http.StatusBadRequest}, errors.New("回调已加密，但该实例未配置 encrypt_key")
		}
		if err := verifyFeishuSignature(header, encryptKey, body); err != nil {
			return WebhookResult{Status: http.StatusUnauthorized}, err
		}
		plain, err := decryptFeishuEvent(encryptKey, envelope.Encrypt)
		if err != nil {
			return WebhookResult{Status: http.StatusBadRequest}, err
		}
		envelope = feishuEnvelope{}
		if err := json.Unmarshal(plain, &envelope); err != nil {
			return WebhookResult{Status: http.StatusBadRequest}, fmt.Errorf("解密后报文解析失败: %w", err)
		}
	}

	// 开放平台配置回调地址时的一次性握手。
	if envelope.Challenge != "" || envelope.Type == "url_verification" {
		return WebhookResult{Status: http.StatusOK, Body: map[string]any{"challenge": envelope.Challenge}}, nil
	}

	token := envelope.Header.Token
	if token == "" {
		token = envelope.Token
	}
	if verifyToken != "" && token != verifyToken {
		return WebhookResult{Status: http.StatusUnauthorized}, errors.New("verification_token 不匹配")
	}

	eventType := envelope.Header.EventType
	if eventType != "im.message.receive_v1" {
		return WebhookResult{Status: http.StatusOK, Body: map[string]any{"code": 0, "msg": "ignored"}}, nil
	}
	if id := envelope.Header.EventID; id != "" && b.alreadySeen(id) {
		return WebhookResult{Status: http.StatusOK, Body: map[string]any{"code": 0, "msg": "duplicate"}}, nil
	}
	msg, ok := parseFeishuMessage(instance.Channel, envelope.Event)
	if !ok {
		return WebhookResult{Status: http.StatusOK, Body: map[string]any{"code": 0, "msg": "skipped"}}, nil
	}
	msg.InstanceID = instanceID
	if !running {
		return WebhookResult{Status: http.StatusServiceUnavailable}, errors.New("IM 接收未启用")
	}
	if err := b.Deliver(ctx, msg); err != nil {
		return WebhookResult{Status: http.StatusServiceUnavailable}, err
	}
	return WebhookResult{Status: http.StatusOK, Body: map[string]any{"code": 0, "msg": "success"}}, nil
}

// alreadySeen 用 event_id 去重。开放平台在超时时会重推同一事件，
// 没有去重就会让 Agent 重复回答。
func (b *Bridge) alreadySeen(eventID string) bool {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = map[string]time.Time{}
	}
	for id, at := range b.seen {
		if now.Sub(at) > dedupeTTL {
			delete(b.seen, id)
		}
	}
	if _, ok := b.seen[eventID]; ok {
		return true
	}
	b.seen[eventID] = now
	return false
}
