package im

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"LongCat-frontend/internal/llm"
	"github.com/skip2/go-qrcode"
)

type persistedBridge struct {
	Enabled   bool              `json:"enabled"`
	Lifecycle string            `json:"lifecycle"`
	Instances []ChannelInstance `json:"instances"`
}

type scanSession struct {
	Channel   RemoteChannelID
	BaseURL   string
	RouteTag  string
	BotType   string
	CreatedAt time.Time
	Refreshes int
}

// Bridge is the provider-neutral desktop bridge state. It owns both the
// control plane (credentials, QR onboarding, persistence) and the data plane
// lifecycle (inbound receive pipeline) so the Web UI can enable/disable
// message reception with a single switch.
type Bridge struct {
	mu          sync.Mutex
	configPath  string
	secretsPath string
	enabled     bool
	lifecycle   string
	running     bool
	lastError   string
	instances   map[string]ChannelInstance
	secrets     map[string]map[string]string
	scans       map[string]*scanSession
	client      *http.Client

	// 数据面：接收管线。running 为 true 时 inbox/dispatcher 有效。
	handler    MessageHandler
	runCancel  context.CancelFunc
	inbox      chan IncomingMessage
	wg         sync.WaitGroup
	transports map[string]*feishuClient   // 发送客户端（tenant_access_token + 回复消息）
	longConns  map[string]*feishuLongConn // 接收长连接（WebSocket，免公网回调）
	// disableLongConn 仅用于单测：跳过长连接的实际建立，避免测试触发真实网络。
	disableLongConn bool
	seen       map[string]time.Time
	// openBaseOverride 仅用于测试，把开放平台域名指向本地 httptest。
	openBaseOverride string
}

func NewBridge() (*Bridge, error) {
	dir, err := llm.ConfigDir()
	if err != nil {
		return nil, err
	}
	return NewBridgeAt(filepath.Join(dir, "im.json"), filepath.Join(dir, "im-secrets.json"))
}

func NewBridgeAt(configPath, secretsPath string) (*Bridge, error) {
	b := &Bridge{configPath: configPath, secretsPath: secretsPath, lifecycle: "attached", instances: map[string]ChannelInstance{}, secrets: map[string]map[string]string{}, scans: map[string]*scanSession{}, client: &http.Client{Timeout: 35 * time.Second}, transports: map[string]*feishuClient{}, longConns: map[string]*feishuLongConn{}, seen: map[string]time.Time{}}
	if err := b.load(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Bridge) load() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if data, err := os.ReadFile(b.configPath); err == nil {
		var saved persistedBridge
		if err := json.Unmarshal(data, &saved); err != nil {
			return fmt.Errorf("解析 IM 配置失败: %w", err)
		}
		b.enabled, b.lifecycle = saved.Enabled, saved.Lifecycle
		if b.lifecycle == "" {
			b.lifecycle = "attached"
		}
		for _, instance := range saved.Instances {
			b.instances[instance.ID] = instance
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if data, err := os.ReadFile(b.secretsPath); err == nil {
		_ = json.Unmarshal(data, &b.secrets)
	} else if !os.IsNotExist(err) {
		return err
	}
	b.refreshCredentialFlagsLocked()
	return nil
}

func (b *Bridge) saveLocked() error {
	instances := make([]ChannelInstance, 0, len(b.instances))
	for _, instance := range b.instances {
		instance.HasCredentials = len(b.secrets[instance.ID]) > 0
		instance.Credentials = ""
		instances = append(instances, instance)
	}
	data, err := json.MarshalIndent(persistedBridge{Enabled: b.enabled, Lifecycle: b.lifecycle, Instances: instances}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.configPath), 0o755); err != nil {
		return err
	}
	tmp := b.configPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.configPath); err != nil {
		return err
	}
	secretData, err := json.MarshalIndent(b.secrets, "", "  ")
	if err != nil {
		return err
	}
	secretTmp := b.secretsPath + ".tmp"
	if err := os.WriteFile(secretTmp, secretData, 0o600); err != nil {
		return err
	}
	return os.Rename(secretTmp, b.secretsPath)
}

func (b *Bridge) refreshCredentialFlagsLocked() {
	for id, instance := range b.instances {
		instance.HasCredentials = len(b.secrets[id]) > 0
		if instance.HasCredentials && (instance.Status == StatusUnconfigured || instance.Status == "") {
			instance.Status = StatusConfigured
		}
		if !instance.HasCredentials {
			instance.Status = StatusUnconfigured
		}
		b.instances[id] = instance
	}
}

func (b *Bridge) Status() BridgeStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshCredentialFlagsLocked()
	state := "stopped"
	if b.running {
		state = "running"
	} else if b.enabled && b.hasReadyInstanceLocked() {
		state = "degraded"
	}
	connected := []ChannelInstance{}
	if b.running {
		// 只列出真正挂上了长连接的实例，而不是“有凭据”就算连上。
		for id := range b.longConns {
			instance, ok := b.instances[id]
			if !ok {
				continue
			}
			tone := StatusConnected
			if instance.LastError != "" {
				tone = StatusError
			}
			instance.Receiving = true
			instance.WebhookPath = WebhookPathPrefix + id
			connected = append(connected, publicInstance(instance, tone))
		}
	}
	transport := ""
	if b.running {
		transport = TransportLongConn
	}
	return BridgeStatus{State: state, Enabled: b.enabled, Lifecycle: b.lifecycle, ConnectedChannels: connected, LastError: b.lastError, Backend: "go-sidecar", Receiving: b.running, Transport: transport}
}

func (b *Bridge) hasReadyInstanceLocked() bool {
	for _, instance := range b.instances {
		if instance.Enabled && instance.HasCredentials {
			return true
		}
	}
	return false
}

func publicInstance(instance ChannelInstance, status StatusTone) ChannelInstance {
	instance.Credentials = ""
	instance.Status = status
	return instance
}

func (b *Bridge) ListInstances() []ChannelInstance {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshCredentialFlagsLocked()
	out := make([]ChannelInstance, 0, len(b.instances))
	for _, instance := range b.instances {
		if instance.HasCredentials {
			instance.Credentials = "im:" + instance.ID
		}
		_, receiving := b.longConns[instance.ID]
		instance.Receiving = receiving
		instance.WebhookPath = WebhookPathPrefix + instance.ID
		out = append(out, publicInstance(instance, instance.Status))
	}
	return out
}

func (b *Bridge) SaveInstance(instance ChannelInstance, credentials map[string]string, connect bool) (ChannelInstance, error) {
	if strings.TrimSpace(instance.ID) == "" {
		instance.ID = fmt.Sprintf("im-%d", time.Now().UnixNano())
	}
	if strings.TrimSpace(instance.Name) == "" {
		instance.Name = string(instance.Channel)
	}
	if instance.Channel == "" {
		return ChannelInstance{}, errors.New("IM 渠道不能为空")
	}
	b.mu.Lock()
	if len(credentials) > 0 {
		merged := b.secrets[instance.ID]
		if merged == nil {
			merged = map[string]string{}
		}
		// 合并而非覆盖：扫码只带回 app_id/app_secret，encrypt_key 等
		// 事件订阅凭据是后续单独填写的，不能被下一次保存清掉。
		for key, value := range credentials {
			if strings.TrimSpace(value) == "" {
				delete(merged, key)
				continue
			}
			merged[key] = value
		}
		b.secrets[instance.ID] = merged
		instance.Credentials = "im:" + instance.ID
	}
	if len(b.secrets[instance.ID]) == 0 {
		instance.Credentials = ""
	}
	instance.HasCredentials = len(b.secrets[instance.ID]) > 0
	if instance.HasCredentials {
		instance.Status = StatusConfigured
	} else {
		instance.Status = StatusUnconfigured
	}
	instance.Receiving, instance.WebhookPath = false, WebhookPathPrefix+instance.ID
	b.instances[instance.ID] = instance
	wasRunning := b.running
	err := b.saveLocked()
	b.mu.Unlock()
	if err != nil {
		return ChannelInstance{}, err
	}
	// 新实例要挂上接收通道必须重建传输表，所以运行中先停再启。
	if connect && instance.Enabled && instance.HasCredentials {
		if wasRunning {
			b.Stop()
		}
		b.Start()
	} else if wasRunning {
		b.Stop()
		b.Start()
	}
	return publicInstance(instance, instance.Status), nil
}

func (b *Bridge) DeleteInstance(id string) error {
	b.mu.Lock()
	if _, ok := b.instances[id]; !ok {
		b.mu.Unlock()
		return errors.New("IM 实例不存在")
	}
	delete(b.instances, id)
	delete(b.secrets, id)
	delete(b.transports, id)
	err := b.saveLocked()
	b.mu.Unlock()
	return err
}

func (b *Bridge) Doctor() map[string]any {
	b.mu.Lock()
	b.refreshCredentialFlagsLocked()
	reports := []map[string]any{}
	for id, instance := range b.instances {
		tone := string(instance.Status)
		hint := ""
		_, receiving := b.longConns[id]
		switch {
		case !instance.HasCredentials:
			tone, hint = string(StatusUnconfigured), "请绑定凭据或使用二维码登录"
		case !instance.Enabled:
			hint = "实例已禁用，启用后才会接收消息"
		case instance.LastError != "":
			tone, hint = string(StatusError), instance.LastError
		case !b.running:
			hint = "IM 接收未启用，点击“启用接收”后生效"
		case !receiving:
			hint = "该渠道暂不支持接收消息"
		default:
			hint = "已通过 WebSocket 长连接接收消息（无需公网回调，扫码时已自动订阅事件）"
		}
		reports = append(reports, map[string]any{
			"name": instance.Name, "channel": instance.Channel,
			"healthy":      instance.HasCredentials && instance.Enabled && receiving && instance.LastError == "",
			"tone":         tone,
			"hint":         hint,
			"receiving":    receiving,
			"webhook_path": WebhookPathPrefix + id,
		})
	}
	state := "stopped"
	if b.running {
		state = "running"
	} else if b.enabled && b.hasReadyInstanceLocked() {
		state = "degraded"
	}
	transport := ""
	if b.running {
		transport = TransportLongConn
	}
	status := BridgeStatus{State: state, Enabled: b.enabled, Lifecycle: b.lifecycle, LastError: b.lastError, Backend: "go-sidecar", Receiving: b.running, Transport: transport, ConnectedChannels: []ChannelInstance{}}
	b.mu.Unlock()
	return map[string]any{"ok": status.LastError == "", "state": status, "instances": reports}
}

func (b *Bridge) ScanBegin(ctx context.Context, channel RemoteChannelID, options map[string]string) (ScanBeginResult, error) {
	switch channel {
	case ChannelFeishu, ChannelLark:
		return b.scanFeishuBegin(ctx, channel)
	case ChannelWeixin:
		return b.scanWeixinBegin(ctx, options)
	default:
		return ScanBeginResult{}, fmt.Errorf("渠道 %s 暂不支持二维码登录", channel)
	}
}

func qrDataURL(value string) (string, error) {
	if strings.HasPrefix(value, "data:image/") {
		return value, nil
	}
	png, err := qrcode.Encode(value, qrcode.Medium, 280)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

func postForm(ctx context.Context, client *http.Client, endpoint string, values url.Values) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("HTTP %s: %w", resp.Status, err)
	}
	// 飞书 app/registration 在 pending / slow_down 等「正常等待态」会返回非 2xx（如 400），
	// 但响应体仍是含 error 字段的合法 JSON。调用方依据 body 内的 error 字段判断状态，
	// 因此这里不按 HTTP 状态码报错（参考 grok-app 的 registrationCall：忽略状态码只看 body）。
	return body, nil
}
