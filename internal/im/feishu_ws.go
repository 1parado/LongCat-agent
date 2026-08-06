package im

import (
	"context"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// feishuLongConn 持有单个飞书/Lark 实例的 WebSocket 长连接，用于接收
// im.message.receive_v1 事件。与 webhook 方式不同，长连接不需要公网回调地址、
// encrypt_key 或 verification_token —— 事件由开放平台直接推送到这条连接上，
// 因此本地运行也能正常收发（这正是 grok-app 的「扫码即通话」实现方式）。
type feishuLongConn struct {
	client     *larkws.Client
	channel    RemoteChannelID
	instanceID string
}

// newFeishuLongConn 建立一条长连接，并把每个收到的消息转交给 deliver（通常
// 是 Bridge.Deliver，从而复用既有的入站队列、ACL 过滤与回复管线）。
// 长连接只需 app_id/app_secret，扫码时已由 Addons 自动订阅了事件与权限。
// instanceID 用于把收到的消息归属到具体实例，否则 Bridge.handleMessage
// 会按空的 InstanceID 查找实例而静默丢弃（导致 bot 不回消息）。
func newFeishuLongConn(instanceID string, channel RemoteChannelID, appID, appSecret string, deliver func(context.Context, IncomingMessage) error) (*feishuLongConn, error) {
	eventHandler := dispatcher.NewEventDispatcher("", "") // 长连接由飞书侧鉴权，无需验签
	eventHandler.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		if msg := feishuEventToIncoming(channel, instanceID, event); msg != nil {
			// 投递失败（队列满/未启用）不应让 SDK 把事件当作处理失败而重推，
			// 这里忽略错误，仅保证不阻断长连接循环。
			_ = deliver(ctx, *msg)
		}
		return nil
	})

	domain := feishuOpenBase(channel) // https://open.feishu.cn 或 https://open.larksuite.com
	client := larkws.NewClient(appID, appSecret,
		larkws.WithDomain(domain),
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelError),
	)
	return &feishuLongConn{client: client, channel: channel, instanceID: instanceID}, nil
}

// run 在 SDK 内部的长连接循环上运行（会自动按策略重连）。该函数会阻塞，
// 调用方应在独立 goroutine 中执行。ctx 取消或 Close 调用后连接断开；底层
// goroutine 会停在 SDK 内部的空 select 上（不占 CPU），随进程退出被回收。
func (c *feishuLongConn) run(ctx context.Context) {
	_ = c.client.Start(ctx)
}

// Close 关闭长连接并停止重连。
func (c *feishuLongConn) Close() {
	c.client.Close()
}

// feishuEventToIncoming 把长连接推送的 im.message.receive_v1 事件归一化为
// 中立的 IncomingMessage。复用既有文本提取与去 @ 占位符逻辑，与 webhook 路径
// 保持一致的解析结果。instanceID 会被写入返回消息，供 Bridge.handleMessage
// 据此归属到具体实例（缺失会导致消息被静默丢弃、bot 不回应）。
func feishuEventToIncoming(channel RemoteChannelID, instanceID string, event *larkim.P2MessageReceiveV1) *IncomingMessage {
	if event == nil || event.Event == nil {
		return nil
	}
	data := event.Event
	if data.Sender != nil && data.Sender.SenderType != nil {
		st := *data.Sender.SenderType
		if st == "bot" || st == "app" {
			return nil // 忽略机器人/应用自身消息，避免回环
		}
	}
	if data.Message == nil {
		return nil
	}
	msg := data.Message
	msgType := derefString(msg.MessageType)
	text := feishuMessageText(msgType, derefString(msg.Content))
	var mentions []feishuMention
	for _, m := range msg.Mentions {
		if m != nil {
			mentions = append(mentions, feishuMention{Key: derefString(m.Key)})
		}
	}
	text = stripFeishuMentions(text, mentions)
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	chatType := derefString(msg.ChatType)
	if chatType == "" {
		chatType = "p2p"
	}
	userID := ""
	if data.Sender != nil && data.Sender.SenderId != nil {
		userID = derefString(data.Sender.SenderId.OpenId)
		if userID == "" {
			userID = derefString(data.Sender.SenderId.UserId)
		}
	}
	return &IncomingMessage{
		InstanceID: instanceID,
		Channel:    channel,
		UserID:     userID,
		ChatID:     derefString(msg.ChatId),
		ChatType:   chatType,
		MessageID:  derefString(msg.MessageId),
		Text:       text,
		Mentioned:  len(msg.Mentions) > 0,
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
