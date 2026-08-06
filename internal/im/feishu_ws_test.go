package im

import (
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func sp(s string) *string { return &s }

// 回归：长连接收到的事件必须带 instance_id，否则 Bridge.handleMessage 按空
// InstanceID 查实例会静默丢弃消息（表现为 bot 收得到但不回话）。
func TestFeishuEventToIncomingSetsInstanceID(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: sp("user"),
				SenderId:   &larkim.UserId{OpenId: sp("ou_user_1")},
			},
			Message: &larkim.EventMessage{
				MessageId:   sp("om_1"),
				ChatId:      sp("oc_1"),
				ChatType:    sp("p2p"),
				MessageType: sp("text"),
				Content:     sp(`{"text":"hello world"}`),
			},
		},
	}

	msg := feishuEventToIncoming(ChannelFeishu, "im-instance-1", event)
	if msg == nil {
		t.Fatal("expected non-nil IncomingMessage for a normal user text message")
	}
	if msg.InstanceID != "im-instance-1" {
		t.Errorf("InstanceID not set: got %q", msg.InstanceID)
	}
	if msg.Channel != ChannelFeishu {
		t.Errorf("Channel: got %q", msg.Channel)
	}
	if msg.UserID != "ou_user_1" {
		t.Errorf("UserID: got %q", msg.UserID)
	}
	if msg.ChatID != "oc_1" {
		t.Errorf("ChatID: got %q", msg.ChatID)
	}
	if msg.ChatType != "p2p" {
		t.Errorf("ChatType: got %q", msg.ChatType)
	}
	if msg.Text != "hello world" {
		t.Errorf("Text: got %q", msg.Text)
	}
	if msg.Mentioned {
		t.Errorf("Mentioned should be false when there are no mentions")
	}
}

// 回归：机器人/应用自身发来的消息应被忽略，避免回环。
func TestFeishuEventToIncomingIgnoresBot(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: sp("bot"),
				SenderId:   &larkim.UserId{OpenId: sp("ou_bot")},
			},
			Message: &larkim.EventMessage{
				MessageId:   sp("om_2"),
				ChatType:    sp("p2p"),
				MessageType: sp("text"),
				Content:     sp(`{"text":"ping"}`),
			},
		},
	}
	if msg := feishuEventToIncoming(ChannelFeishu, "im-instance-1", event); msg != nil {
		t.Fatalf("expected nil for bot sender, got %+v", msg)
	}
}

// 回归：空文本（如纯图片/表情且无可提取文本）不应触发回复。
func TestFeishuEventToIncomingIgnoresEmptyText(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: sp("user"),
				SenderId:   &larkim.UserId{OpenId: sp("ou_user_1")},
			},
			Message: &larkim.EventMessage{
				MessageId:   sp("om_3"),
				ChatType:    sp("p2p"),
				MessageType: sp("text"),
				Content:     sp(`{"text":"   "}`),
			},
		},
	}
	if msg := feishuEventToIncoming(ChannelFeishu, "im-instance-1", event); msg != nil {
		t.Fatalf("expected nil for empty text, got %+v", msg)
	}
}
