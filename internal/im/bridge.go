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

// Bridge is the provider-neutral desktop bridge state. Connector transports
// can attach to this lifecycle without exposing credentials to the Web UI.
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
}

func NewBridge() (*Bridge, error) {
	dir, err := llm.ConfigDir()
	if err != nil {
		return nil, err
	}
	return NewBridgeAt(filepath.Join(dir, "im.json"), filepath.Join(dir, "im-secrets.json"))
}

func NewBridgeAt(configPath, secretsPath string) (*Bridge, error) {
	b := &Bridge{configPath: configPath, secretsPath: secretsPath, lifecycle: "attached", instances: map[string]ChannelInstance{}, secrets: map[string]map[string]string{}, scans: map[string]*scanSession{}, client: &http.Client{Timeout: 35 * time.Second}}
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
		for _, instance := range b.instances {
			if instance.Enabled && instance.HasCredentials {
				connected = append(connected, publicInstance(instance, StatusConnected))
			}
		}
	}
	return BridgeStatus{State: state, Enabled: b.enabled, Lifecycle: b.lifecycle, ConnectedChannels: connected, LastError: b.lastError, Backend: "go-sidecar"}
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
		out = append(out, publicInstance(instance, instance.Status))
	}
	return out
}

func (b *Bridge) Start() BridgeStatus {
	b.mu.Lock()
	b.enabled, b.running, b.lastError = true, true, ""
	_ = b.saveLocked()
	b.mu.Unlock()
	return b.Status()
}
func (b *Bridge) Stop() BridgeStatus {
	b.mu.Lock()
	b.running, b.enabled = false, false
	_ = b.saveLocked()
	b.mu.Unlock()
	return b.Status()
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
	defer b.mu.Unlock()
	if len(credentials) > 0 {
		b.secrets[instance.ID] = credentials
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
	b.instances[instance.ID] = instance
	if connect && instance.Enabled && instance.HasCredentials {
		b.enabled, b.running = true, true
	}
	if err := b.saveLocked(); err != nil {
		return ChannelInstance{}, err
	}
	return publicInstance(instance, instance.Status), nil
}

func (b *Bridge) DeleteInstance(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.instances[id]; !ok {
		return errors.New("IM 实例不存在")
	}
	delete(b.instances, id)
	delete(b.secrets, id)
	return b.saveLocked()
}

func (b *Bridge) Doctor() map[string]any {
	b.mu.Lock()
	b.refreshCredentialFlagsLocked()
	reports := []map[string]any{}
	for _, instance := range b.instances {
		tone := string(instance.Status)
		hint := ""
		if !instance.HasCredentials {
			tone, hint = string(StatusUnconfigured), "请绑定凭据或使用二维码登录"
		}
		reports = append(reports, map[string]any{"name": instance.Name, "channel": instance.Channel, "healthy": instance.HasCredentials && instance.Enabled, "tone": tone, "hint": hint})
	}
	state := "stopped"
	if b.running {
		state = "running"
	} else if b.enabled && b.hasReadyInstanceLocked() {
		state = "degraded"
	}
	status := BridgeStatus{State: state, Enabled: b.enabled, Lifecycle: b.lifecycle, LastError: b.lastError, Backend: "go-sidecar"}
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return body, nil
}
