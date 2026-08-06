package im

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 飞书的 app/registration?action=poll 在「待确认 / 轮询过快」时会用 HTTP 400 返回，
// 但 body 仍是合法 JSON（error 字段）。历史上 postForm 把非 2xx 当作致命错误，
// 导致 ScanPoll 第一次 poll 就报错、前端停止轮询、永远卡在「等待扫码确认」。
// 这些测试确保：非 2xx + 合法 JSON 时 ScanPoll 不再报错，而是按 body 内容判状态。

func newScanTestBridge(t *testing.T, handler http.HandlerFunc) (*Bridge, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	b := &Bridge{
		scans:  map[string]*scanSession{},
		client: &http.Client{},
	}
	b.scans["code1"] = &scanSession{Channel: ChannelFeishu, BaseURL: srv.URL}
	return b, srv.Close
}

func TestScanPollToleratesNon2xxPending(t *testing.T) {
	b, closeFn := newScanTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"authorization_pending","code":20094}`))
	})
	defer closeFn()

	res, err := b.ScanPoll(context.Background(), ChannelFeishu, "code1")
	if err != nil {
		t.Fatalf("expected no error for HTTP 400 pending, got %v", err)
	}
	if res.Status != "pending" {
		t.Fatalf("expected status=pending, got %q", res.Status)
	}
}

func TestScanPollToleratesNon2xxSlowDown(t *testing.T) {
	b, closeFn := newScanTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"slow_down","error_description":"too frequent","code":20095}`))
	})
	defer closeFn()

	res, err := b.ScanPoll(context.Background(), ChannelFeishu, "code1")
	if err != nil {
		t.Fatalf("expected no error for HTTP 400 slow_down, got %v", err)
	}
	if res.Status != "pending" {
		t.Fatalf("expected status=pending, got %q", res.Status)
	}
	if !res.SlowDown {
		t.Fatalf("expected SlowDown=true for slow_down response")
	}
}

func TestScanPollCompletedOnNon2xx(t *testing.T) {
	b, closeFn := newScanTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"client_id":"cli_test","client_secret":"sec_test","user_info":{"open_id":"ou_abc","tenant_brand":"feishu"}}`))
	})
	defer closeFn()

	res, err := b.ScanPoll(context.Background(), ChannelFeishu, "code1")
	if err != nil {
		t.Fatalf("expected no error for HTTP 400 completed, got %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("expected status=completed, got %q", res.Status)
	}
	if res.AppID != "cli_test" || res.AppSecret != "sec_test" {
		t.Fatalf("expected credentials, got app_id=%q app_secret=%q", res.AppID, res.AppSecret)
	}
}

// TestFeishuAddonsEncodeRoundTrip 验证 Addons 的编码方式（gzip + base64url）
// 与飞书官方 SDK（scene/registration/addons.go）一致：解码后能还原出期望的
// 权限作用域与事件订阅结构。扫码时这段内容会挂在二维码 URL 的 ?addons= 上，
// 开放平台据此自动订阅 im.message.receive_v1 并申请 im:message 权限。
func TestFeishuAddonsEncodeRoundTrip(t *testing.T) {
	encoded, err := feishuAddonsEncode(defaultFeishuAddons())
	if err != nil {
		t.Fatalf("feishuAddonsEncode failed: %v", err)
	}
	if encoded == "" {
		t.Fatal("expected non-empty addons encoding")
	}

	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("addons 不是合法的 base64url: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("addons 不是合法的 gzip: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gzip 解压失败: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(decoded, &got); err != nil {
		t.Fatalf("addons 解压后不是合法 JSON: %v", err)
	}
	scopes, ok := got["scopes"].(map[string]any)
	if !ok {
		t.Fatalf("addons 缺少 scopes: %v", got)
	}
	tenantScopes, ok := scopes["tenant"].([]any)
	if !ok || len(tenantScopes) != 1 || tenantScopes[0] != "im:message" {
		t.Fatalf("scopes.tenant 期望 [im:message], 实际 %v", scopes["tenant"])
	}
	events, ok := got["events"].(map[string]any)
	if !ok {
		t.Fatalf("addons 缺少 events: %v", got)
	}
	items, ok := events["items"].(map[string]any)
	if !ok {
		t.Fatalf("events 缺少 items: %v", events)
	}
	tenantEvents, ok := items["tenant"].([]any)
	if !ok || len(tenantEvents) != 1 || tenantEvents[0] != "im.message.receive_v1" {
		t.Fatalf("events.items.tenant 期望 [im.message.receive_v1], 实际 %v", items["tenant"])
	}
}
