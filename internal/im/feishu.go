package im

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// feishuOpenBase 返回开放平台 API 域名。飞书与 Lark 共用同一套接口，只是域名不同。
func feishuOpenBase(channel RemoteChannelID) string {
	if channel == ChannelLark {
		return "https://open.larksuite.com"
	}
	return "https://open.feishu.cn"
}

// feishuClient 是单个飞书/Lark 机器人实例的数据面客户端：
// 负责 tenant_access_token 的获取与缓存，以及向会话发送消息。
// 控制面（扫码、凭据持久化）仍由 Bridge 负责。
type feishuClient struct {
	base      string
	appID     string
	appSecret string
	http      *http.Client

	mu       sync.Mutex
	token    string
	expireAt time.Time
}

func newFeishuClient(channel RemoteChannelID, appID, appSecret string, hc *http.Client) *feishuClient {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &feishuClient{base: feishuOpenBase(channel), appID: appID, appSecret: appSecret, http: hc}
}

// tenantToken 返回可用的 tenant_access_token，提前 5 分钟过期以避免边界失败。
func (c *feishuClient) tenantToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.expireAt) {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"app_id": c.appID, "app_secret": c.appSecret})
	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := c.postJSON(ctx, "/open-apis/auth/v3/tenant_access_token/internal", "", body, &out); err != nil {
		return "", err
	}
	if out.Code != 0 || out.Token == "" {
		return "", fmt.Errorf("获取 tenant_access_token 失败: code=%d %s", out.Code, out.Msg)
	}
	ttl := out.Expire
	if ttl <= 0 {
		ttl = 7200
	}
	c.mu.Lock()
	c.token, c.expireAt = out.Token, time.Now().Add(time.Duration(ttl)*time.Second-5*time.Minute)
	c.mu.Unlock()
	return out.Token, nil
}

// Verify 主动换取一次 token，用来验证凭据是否仍然有效。
func (c *feishuClient) Verify(ctx context.Context) error {
	_, err := c.tenantToken(ctx)
	return err
}

// ReplyText 以“回复”的形式回到原消息所在会话；messageID 为空时退回普通发送。
func (c *feishuClient) ReplyText(ctx context.Context, messageID, chatID, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	token, err := c.tenantToken(ctx)
	if err != nil {
		return err
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	if messageID != "" {
		payload, _ := json.Marshal(map[string]string{"content": string(content), "msg_type": "text"})
		var out feishuAPIResult
		if err := c.postJSON(ctx, "/open-apis/im/v1/messages/"+messageID+"/reply", token, payload, &out); err == nil && out.Code == 0 {
			return nil
		}
		// 回复失败（消息过期/被撤回等）时退回普通发送，保证用户能收到结果。
	}
	if chatID == "" {
		return errors.New("缺少会话 ID，无法发送消息")
	}
	payload, _ := json.Marshal(map[string]string{"receive_id": chatID, "msg_type": "text", "content": string(content)})
	var out feishuAPIResult
	if err := c.postJSON(ctx, "/open-apis/im/v1/messages?receive_id_type=chat_id", token, payload, &out); err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("发送飞书消息失败: code=%d %s", out.Code, out.Msg)
	}
	return nil
}

type feishuAPIResult struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func (c *feishuClient) postJSON(ctx context.Context, path, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// ---------- 事件回调（事件订阅 / Webhook）----------

// feishuEnvelope 同时兼容 v1(schema 空) 与 v2(schema=2.0) 事件格式，
// 以及加密模式下只有 encrypt 字段的信封。
type feishuEnvelope struct {
	Encrypt   string `json:"encrypt"`
	Schema    string `json:"schema"`
	Challenge string `json:"challenge"`
	Type      string `json:"type"`
	Token     string `json:"token"`
	Header    struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		Token     string `json:"token"`
		AppID     string `json:"app_id"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`
}

// decryptFeishuEvent 解密事件订阅的 encrypt 字段。
// 算法：key = sha256(encryptKey)，密文 base64 解码后前 16 字节为 IV，AES-256-CBC + PKCS#7。
func decryptFeishuEvent(encryptKey, encrypted string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encrypted))
	if err != nil {
		return nil, fmt.Errorf("事件密文 base64 解码失败: %w", err)
	}
	if len(raw) <= aes.BlockSize || len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("事件密文长度非法")
	}
	sum := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	iv, body := raw[:aes.BlockSize], raw[aes.BlockSize:]
	plain := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, body)
	pad := int(plain[len(plain)-1])
	if pad <= 0 || pad > aes.BlockSize || pad > len(plain) {
		return nil, errors.New("事件密文填充非法")
	}
	return plain[:len(plain)-pad], nil
}

// feishuSignature 计算事件回调签名：sha256(timestamp + nonce + encryptKey + body)。
func feishuSignature(timestamp, nonce, encryptKey string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(timestamp))
	h.Write([]byte(nonce))
	h.Write([]byte(encryptKey))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// verifyFeishuSignature 在配置了 encrypt_key 时做常数时间比较。
func verifyFeishuSignature(header http.Header, encryptKey string, body []byte) error {
	got := header.Get("X-Lark-Signature")
	if got == "" {
		return errors.New("缺少 X-Lark-Signature 签名头")
	}
	want := feishuSignature(header.Get("X-Lark-Request-Timestamp"), header.Get("X-Lark-Request-Nonce"), encryptKey, body)
	if !hmac.Equal([]byte(got), []byte(want)) {
		return errors.New("事件签名校验失败")
	}
	return nil
}

type feishuMention struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	ID   struct {
		OpenID string `json:"open_id"`
	} `json:"id"`
}

type feishuMessageEvent struct {
	Sender struct {
		SenderID struct {
			OpenID string `json:"open_id"`
			UserID string `json:"user_id"`
		} `json:"sender_id"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
	Message struct {
		MessageID   string          `json:"message_id"`
		ChatID      string          `json:"chat_id"`
		ChatType    string          `json:"chat_type"`
		MessageType string          `json:"message_type"`
		Content     string          `json:"content"`
		Mentions    []feishuMention `json:"mentions"`
	} `json:"message"`
}

// parseFeishuMessage 把 im.message.receive_v1 事件转换成中立的 IncomingMessage。
// 只处理机器人能理解的文本类消息；其它类型返回 ok=false 由上层忽略。
func parseFeishuMessage(channel RemoteChannelID, raw json.RawMessage) (IncomingMessage, bool) {
	var ev feishuMessageEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return IncomingMessage{}, false
	}
	if ev.Sender.SenderType == "bot" || ev.Sender.SenderType == "app" {
		return IncomingMessage{}, false // 忽略机器人自身/其它应用的消息，避免回环
	}
	text := feishuMessageText(ev.Message.MessageType, ev.Message.Content)
	text = stripFeishuMentions(text, ev.Message.Mentions)
	if strings.TrimSpace(text) == "" {
		return IncomingMessage{}, false
	}
	chatType := ev.Message.ChatType
	if chatType == "" {
		chatType = "p2p"
	}
	userID := ev.Sender.SenderID.OpenID
	if userID == "" {
		userID = ev.Sender.SenderID.UserID
	}
	return IncomingMessage{
		Channel:   channel,
		UserID:    userID,
		ChatID:    ev.Message.ChatID,
		ChatType:  chatType,
		MessageID: ev.Message.MessageID,
		Text:      strings.TrimSpace(text),
		Mentioned: len(ev.Message.Mentions) > 0,
	}, true
}

// feishuMessageText 从消息 content（JSON 字符串）中抽取纯文本。
func feishuMessageText(messageType, content string) string {
	switch messageType {
	case "text":
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err != nil {
			return ""
		}
		return payload.Text
	case "post":
		var payload struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err != nil {
			return ""
		}
		var b strings.Builder
		b.WriteString(payload.Title)
		for _, line := range payload.Content {
			for _, node := range line {
				if node.Tag == "text" || node.Tag == "a" {
					b.WriteString(node.Text)
				}
			}
			b.WriteString("\n")
		}
		return b.String()
	default:
		return ""
	}
}

// stripFeishuMentions 去掉 "@_user_1" 这类占位符，保留用户真正输入的内容。
func stripFeishuMentions(text string, mentions []feishuMention) string {
	for _, m := range mentions {
		if m.Key != "" {
			text = strings.ReplaceAll(text, m.Key, "")
		}
	}
	return strings.TrimSpace(text)
}
