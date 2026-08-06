package im

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// encryptFeishuEvent 是测试用的对称实现，用来验证解密逻辑。
func encryptFeishuEvent(t *testing.T, key string, plain []byte) string {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte{}, plain...), strings.Repeat(string(rune(pad)), pad)...)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("iv: %v", err)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(append(iv, out...))
}

func TestDecryptFeishuEvent(t *testing.T) {
	const key = "test-encrypt-key"
	want := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1"}}`
	got, err := decryptFeishuEvent(key, encryptFeishuEvent(t, key, []byte(want)))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != want {
		t.Fatalf("解密结果不匹配:\n got=%s\nwant=%s", got, want)
	}
}

func TestDecryptFeishuEventRejectsGarbage(t *testing.T) {
	if _, err := decryptFeishuEvent("k", "not-base64!!"); err == nil {
		t.Fatal("非法密文应当报错")
	}
	if _, err := decryptFeishuEvent("k", base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("长度非法的密文应当报错")
	}
}

func TestVerifyFeishuSignature(t *testing.T) {
	const key = "enc"
	body := []byte(`{"encrypt":"x"}`)
	header := http.Header{}
	header.Set("X-Lark-Request-Timestamp", "1700000000")
	header.Set("X-Lark-Request-Nonce", "nonce")
	header.Set("X-Lark-Signature", feishuSignature("1700000000", "nonce", key, body))
	if err := verifyFeishuSignature(header, key, body); err != nil {
		t.Fatalf("合法签名应通过: %v", err)
	}
	header.Set("X-Lark-Signature", "deadbeef")
	if err := verifyFeishuSignature(header, key, body); err == nil {
		t.Fatal("错误签名应当被拒绝")
	}
	if err := verifyFeishuSignature(http.Header{}, key, body); err == nil {
		t.Fatal("缺少签名头应当被拒绝")
	}
}

func TestParseFeishuMessageStripsMentions(t *testing.T) {
	event := json.RawMessage(`{
      "sender":{"sender_id":{"open_id":"ou_user"},"sender_type":"user"},
      "message":{"message_id":"om_1","chat_id":"oc_1","chat_type":"group","message_type":"text",
        "content":"{\"text\":\"@_user_1 帮我看下构建\"}",
        "mentions":[{"key":"@_user_1","name":"bot","id":{"open_id":"ou_bot"}}]}}`)
	msg, ok := parseFeishuMessage(ChannelFeishu, event)
	if !ok {
		t.Fatal("应当解析成功")
	}
	if msg.Text != "帮我看下构建" {
		t.Fatalf("mention 占位符未清理: %q", msg.Text)
	}
	if !msg.Mentioned || msg.ChatType != "group" || msg.ChatID != "oc_1" || msg.MessageID != "om_1" || msg.UserID != "ou_user" {
		t.Fatalf("字段映射错误: %+v", msg)
	}
}

func TestParseFeishuMessageIgnoresBotAndEmpty(t *testing.T) {
	bot := json.RawMessage(`{"sender":{"sender_id":{"open_id":"ou_b"},"sender_type":"bot"},
      "message":{"message_id":"om","chat_id":"oc","chat_type":"p2p","message_type":"text","content":"{\"text\":\"hi\"}"}}`)
	if _, ok := parseFeishuMessage(ChannelFeishu, bot); ok {
		t.Fatal("机器人自身消息应被忽略，避免回环")
	}
	image := json.RawMessage(`{"sender":{"sender_id":{"open_id":"ou_u"},"sender_type":"user"},
      "message":{"message_id":"om","chat_id":"oc","chat_type":"p2p","message_type":"image","content":"{\"image_key\":\"k\"}"}}`)
	if _, ok := parseFeishuMessage(ChannelFeishu, image); ok {
		t.Fatal("非文本消息应被忽略")
	}
}

func TestFeishuOpenBase(t *testing.T) {
	if got := feishuOpenBase(ChannelLark); got != "https://open.larksuite.com" {
		t.Fatalf("Lark 域名错误: %s", got)
	}
	if got := feishuOpenBase(ChannelFeishu); got != "https://open.feishu.cn" {
		t.Fatalf("Feishu 域名错误: %s", got)
	}
}
