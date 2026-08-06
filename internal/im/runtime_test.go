package im

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestBridge(t *testing.T, openBase string) *Bridge {
	t.Helper()
	dir := t.TempDir()
	b, err := NewBridgeAt(filepath.Join(dir, "im.json"), filepath.Join(dir, "im-secrets.json"))
	if err != nil {
		t.Fatalf("NewBridgeAt: %v", err)
	}
	b.openBaseOverride = openBase
	return b
}

// fakeFeishu 模拟开放平台的 token 与消息接口。
func fakeFeishu(t *testing.T, replies chan<- string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "t-test", "expire": 7200})
	})
	mux.HandleFunc("/open-apis/im/v1/messages/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(body, &payload)
		select {
		case replies <- payload.Content:
		default:
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func saveFeishuInstance(t *testing.T, b *Bridge, id string) {
	t.Helper()
	_, err := b.SaveInstance(ChannelInstance{ID: id, Channel: ChannelFeishu, Name: "bot", Enabled: true}, map[string]string{"app_id": "cli_x", "app_secret": "sec"}, false)
	if err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
}

func messageEventBody(eventID, text string) []byte {
	envelope := map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_id": eventID, "event_type": "im.message.receive_v1", "token": ""},
		"event": map[string]any{
			"sender":  map[string]any{"sender_id": map[string]any{"open_id": "ou_user"}, "sender_type": "user"},
			"message": map[string]any{"message_id": "om_1", "chat_id": "oc_1", "chat_type": "p2p", "message_type": "text", "content": `{"text":"` + text + `"}`},
		},
	}
	body, _ := json.Marshal(envelope)
	return body
}

func TestStartStopControlsReceiving(t *testing.T) {
	srv := fakeFeishu(t, make(chan string, 1))
	b := newTestBridge(t, srv.URL)
	b.disableLongConn = true // 单测不真正建立 WebSocket，避免触发真实网络
	saveFeishuInstance(t, b, "i1")

	if status := b.Status(); status.Receiving {
		t.Fatal("保存实例后不应自动开始接收")
	}
	status := b.Start()
	if !status.Receiving || status.State != "running" || status.Transport != TransportLongConn {
		t.Fatalf("Start 后应处于接收状态: %+v", status)
	}
	if len(status.ConnectedChannels) != 1 {
		t.Fatalf("应有 1 个已挂载实例: %+v", status.ConnectedChannels)
	}
	status = b.Stop()
	if status.Receiving || status.Enabled || status.State != "stopped" {
		t.Fatalf("Stop 后应完全停止: %+v", status)
	}
	if err := b.Deliver(context.Background(), IncomingMessage{InstanceID: "i1"}); err == nil {
		t.Fatal("停止后投递应当报错，而不是阻塞")
	}
}

func TestStartWithoutInstancesReportsReason(t *testing.T) {
	b := newTestBridge(t, "")
	status := b.Start()
	defer b.Stop()
	if status.LastError == "" {
		t.Fatal("没有可用实例时应给出明确原因")
	}
}

func TestWebhookURLVerification(t *testing.T) {
	srv := fakeFeishu(t, make(chan string, 1))
	b := newTestBridge(t, srv.URL)
	saveFeishuInstance(t, b, "i1")
	body := []byte(`{"challenge":"abc123","token":"v","type":"url_verification"}`)
	res, err := b.HandleWebhook(context.Background(), "i1", http.Header{}, body, true)
	if err != nil {
		t.Fatalf("握手不应报错: %v", err)
	}
	if res.Status != http.StatusOK || res.Body["challenge"] != "abc123" {
		t.Fatalf("握手响应错误: %+v", res)
	}
}

func TestWebhookRoutesMessageToHandlerAndReplies(t *testing.T) {
	replies := make(chan string, 4)
	srv := fakeFeishu(t, replies)
	b := newTestBridge(t, srv.URL)
	saveFeishuInstance(t, b, "i1")

	got := make(chan IncomingMessage, 1)
	b.SetHandler(func(_ context.Context, msg IncomingMessage) (string, error) {
		got <- msg
		return "pong", nil
	})
	b.Start()
	defer b.Stop()

	res, err := b.HandleWebhook(context.Background(), "i1", http.Header{}, messageEventBody("ev-1", "ping"), true)
	if err != nil || res.Status != http.StatusOK {
		t.Fatalf("回调处理失败: %+v err=%v", res, err)
	}
	select {
	case msg := <-got:
		if msg.Text != "ping" || msg.InstanceID != "i1" || msg.ChatID != "oc_1" {
			t.Fatalf("入站消息字段错误: %+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("处理器未在超时内收到消息")
	}
	select {
	case content := <-replies:
		if !strings.Contains(content, "pong") {
			t.Fatalf("回复内容错误: %s", content)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未向飞书回投回复")
	}
}

func TestWebhookDeduplicatesRetries(t *testing.T) {
	replies := make(chan string, 4)
	srv := fakeFeishu(t, replies)
	b := newTestBridge(t, srv.URL)
	saveFeishuInstance(t, b, "i1")
	calls := make(chan struct{}, 4)
	b.SetHandler(func(_ context.Context, _ IncomingMessage) (string, error) {
		calls <- struct{}{}
		return "", nil
	})
	b.Start()
	defer b.Stop()

	body := messageEventBody("ev-dup", "hi")
	for i := 0; i < 3; i++ {
		if _, err := b.HandleWebhook(context.Background(), "i1", http.Header{}, body, true); err != nil {
			t.Fatalf("第 %d 次回调失败: %v", i, err)
		}
	}
	select {
	case <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("首次事件未被处理")
	}
	select {
	case <-calls:
		t.Fatal("重复 event_id 应被去重")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWebhookRejectsUntrustedWithoutSecrets(t *testing.T) {
	b := newTestBridge(t, "")
	saveFeishuInstance(t, b, "i1")
	res, err := b.HandleWebhook(context.Background(), "i1", http.Header{}, messageEventBody("ev-2", "hi"), false)
	if err == nil || res.Status != http.StatusForbidden {
		t.Fatalf("未配置校验凭据的外部回调应被拒绝: %+v err=%v", res, err)
	}
}

func TestWebhookVerificationTokenMismatch(t *testing.T) {
	b := newTestBridge(t, "")
	if _, err := b.SaveInstance(ChannelInstance{ID: "i1", Channel: ChannelFeishu, Name: "bot", Enabled: true},
		map[string]string{"app_id": "a", "app_secret": "s", "verification_token": "right"}, false); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	body := []byte(`{"schema":"2.0","header":{"event_id":"e","event_type":"im.message.receive_v1","token":"wrong"},"event":{}}`)
	res, err := b.HandleWebhook(context.Background(), "i1", http.Header{}, body, true)
	if err == nil || res.Status != http.StatusUnauthorized {
		t.Fatalf("token 不匹配应被拒绝: %+v err=%v", res, err)
	}
}

func TestSaveInstanceMergesSecrets(t *testing.T) {
	b := newTestBridge(t, "")
	saveFeishuInstance(t, b, "i1")
	if _, err := b.SaveInstance(ChannelInstance{ID: "i1", Channel: ChannelFeishu, Name: "bot", Enabled: true},
		map[string]string{"encrypt_key": "ek"}, false); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	b.mu.Lock()
	secret := b.secrets["i1"]
	b.mu.Unlock()
	if secret["app_id"] != "cli_x" || secret["app_secret"] != "sec" || secret["encrypt_key"] != "ek" {
		t.Fatalf("补填事件订阅凭据不应覆盖扫码凭据: %+v", secret)
	}
}

func TestACLAllows(t *testing.T) {
	group := IncomingMessage{ChatType: "group", UserID: "ou_a", ChatID: "oc_a"}
	p2p := IncomingMessage{ChatType: "p2p", UserID: "ou_a", ChatID: "oc_a"}

	if !aclAllows(ACLConfig{}, group) || !aclAllows(ACLConfig{}, p2p) {
		t.Fatal("空 ACL 应当默认放行")
	}
	if aclAllows(ACLConfig{GroupOnly: true}, p2p) {
		t.Fatal("GroupOnly 应拒绝私聊")
	}
	if aclAllows(ACLConfig{RequireMention: true}, group) {
		t.Fatal("群聊未 @ 时应被忽略")
	}
	if !aclAllows(ACLConfig{RequireMention: true}, p2p) {
		t.Fatal("私聊不应要求 @")
	}
	mentioned := group
	mentioned.Mentioned = true
	if !aclAllows(ACLConfig{RequireMention: true}, mentioned) {
		t.Fatal("群聊已 @ 应放行")
	}
	if aclAllows(ACLConfig{AllowFrom: "ou_b, ou_c"}, group) {
		t.Fatal("不在白名单的用户应被拒绝")
	}
	if !aclAllows(ACLConfig{AllowFrom: "ou_b, ou_a"}, group) {
		t.Fatal("白名单内的用户应放行")
	}
	if aclAllows(ACLConfig{AllowChat: "oc_x"}, group) {
		t.Fatal("不在白名单的会话应被拒绝")
	}
}
