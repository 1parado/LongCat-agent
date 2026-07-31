package im

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (b *Bridge) scanFeishuBegin(ctx context.Context, channel RemoteChannelID) (ScanBeginResult, error) {
	base := "https://accounts.feishu.cn"
	if channel == ChannelLark {
		base = "https://accounts.larksuite.com"
	}
	initData, err := postForm(ctx, b.client, base+"/oauth/v1/app/registration", url.Values{"action": {"init"}})
	if err != nil {
		return ScanBeginResult{}, fmt.Errorf("MCP/IM OAuth init 失败: %w", err)
	}
	if message, _ := initData["error"].(string); message != "" {
		return ScanBeginResult{}, fmt.Errorf("%s: %v", message, initData["error_description"])
	}
	data, err := postForm(ctx, b.client, base+"/oauth/v1/app/registration", url.Values{"action": {"begin"}, "archetype": {"PersonalAgent"}, "auth_method": {"client_secret"}, "request_user_info": {"open_id"}})
	if err != nil {
		return ScanBeginResult{}, err
	}
	if message, _ := data["error"].(string); message != "" {
		return ScanBeginResult{}, fmt.Errorf("%s: %v", message, data["error_description"])
	}
	code, _ := data["device_code"].(string)
	rawURI, _ := data["verification_uri_complete"].(string)
	if code == "" || rawURI == "" {
		return ScanBeginResult{}, fmt.Errorf("OAuth 返回缺少二维码信息")
	}
	image, err := qrDataURL(rawURI)
	if err != nil {
		return ScanBeginResult{}, err
	}
	interval := int(number(data["interval"], 5))
	expiry := int(number(data["expire_in"], 600))
	b.mu.Lock()
	b.scans[code] = &scanSession{Channel: channel, CreatedAt: time.Now()}
	b.mu.Unlock()
	return ScanBeginResult{DeviceCode: code, VerificationURI: image, IntervalSec: interval, ExpireInSec: expiry, Platform: string(channel)}, nil
}

func (b *Bridge) scanWeixinBegin(ctx context.Context, options map[string]string) (ScanBeginResult, error) {
	base := strings.TrimRight(options["base_url"], "/")
	if base == "" {
		base = "https://ilinkai.weixin.qq.com"
	}
	route := options["route_tag"]
	botType := options["bot_type"]
	if botType == "" {
		botType = "3"
	}
	data, err := b.getJSON(ctx, base+"/ilink/bot/get_bot_qrcode?bot_type="+url.QueryEscape(botType), route)
	if err != nil {
		return ScanBeginResult{}, err
	}
	code, _ := data["qrcode"].(string)
	rawURI, _ := data["qrcode_img_content"].(string)
	if code == "" || rawURI == "" {
		return ScanBeginResult{}, errors.New("Weixin 返回缺少二维码信息")
	}
	image, err := qrDataURL(rawURI)
	if err != nil {
		return ScanBeginResult{}, err
	}
	b.mu.Lock()
	b.scans[code] = &scanSession{Channel: ChannelWeixin, BaseURL: base, RouteTag: route, BotType: botType, CreatedAt: time.Now()}
	b.mu.Unlock()
	return ScanBeginResult{DeviceCode: code, VerificationURI: image, IntervalSec: 2, ExpireInSec: 480, Platform: string(ChannelWeixin)}, nil
}

func (b *Bridge) ScanPoll(ctx context.Context, channel RemoteChannelID, code string) (ScanPollResult, error) {
	b.mu.Lock()
	session, ok := b.scans[code]
	if ok {
		clone := *session
		session = &clone
	}
	b.mu.Unlock()
	if !ok {
		return ScanPollResult{}, errors.New("二维码会话不存在或已过期")
	}
	if session.Channel != channel {
		return ScanPollResult{}, errors.New("二维码渠道不匹配")
	}
	if channel == ChannelWeixin {
		return b.pollWeixin(ctx, code, session)
	}
	base := session.BaseURL
	if base == "" {
		base = "https://accounts.feishu.cn"
		if channel == ChannelLark {
			base = "https://accounts.larksuite.com"
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		data, err := postForm(ctx, b.client, base+"/oauth/v1/app/registration", url.Values{"action": {"poll"}, "device_code": {code}})
		if err != nil {
			return ScanPollResult{}, err
		}
		user, _ := data["user_info"].(map[string]any)
		brand, _ := user["tenant_brand"].(string)
		if strings.EqualFold(brand, "lark") && base != "https://accounts.larksuite.com" {
			base = "https://accounts.larksuite.com"
			b.mu.Lock()
			if current, ok := b.scans[code]; ok {
				current.BaseURL = base
			}
			b.mu.Unlock()
			continue
		}
		appID, _ := data["client_id"].(string)
		secret, _ := data["client_secret"].(string)
		platform := string(channel)
		if strings.EqualFold(brand, "lark") {
			platform = string(ChannelLark)
		} else if strings.EqualFold(brand, "feishu") {
			platform = string(ChannelFeishu)
		}
		if appID != "" && secret != "" {
			b.finishScan(code)
			owner, _ := user["open_id"].(string)
			return ScanPollResult{Status: "completed", AppID: appID, AppSecret: secret, OwnerOpenID: owner, Platform: platform}, nil
		}
		errorCode, _ := data["error"].(string)
		switch errorCode {
		case "", "authorization_pending", "slow_down":
			return ScanPollResult{Status: "pending", Platform: platform}, nil
		case "access_denied":
			return ScanPollResult{Status: "denied", Error: "授权被拒绝", Platform: platform}, nil
		case "expired_token":
			return ScanPollResult{Status: "expired", Error: "二维码已过期", Platform: platform}, nil
		default:
			return ScanPollResult{Status: "error", Error: errorCode, Platform: platform}, nil
		}
	}
	return ScanPollResult{Status: "pending", Platform: string(channel)}, nil
}

func (b *Bridge) pollWeixin(ctx context.Context, code string, session *scanSession) (ScanPollResult, error) {
	if time.Since(session.CreatedAt) > 90*time.Second && session.Refreshes < 5 {
		data, err := b.getJSON(ctx, session.BaseURL+"/ilink/bot/get_bot_qrcode?bot_type="+url.QueryEscape(session.BotType), session.RouteTag)
		if err == nil {
			newCode, _ := data["qrcode"].(string)
			rawURI, _ := data["qrcode_img_content"].(string)
			if newCode != "" && rawURI != "" {
				image, imageErr := qrDataURL(rawURI)
				if imageErr != nil {
					return ScanPollResult{}, imageErr
				}
				b.mu.Lock()
				delete(b.scans, code)
				session.CreatedAt, session.Refreshes = time.Now(), session.Refreshes+1
				b.scans[newCode] = session
				b.mu.Unlock()
				return ScanPollResult{Status: "pending", Error: "qr_refreshed", VerificationURI: image, DeviceCode: newCode, Platform: string(ChannelWeixin)}, nil
			}
		}
	}
	data, err := b.getJSON(ctx, session.BaseURL+"/ilink/bot/get_qrcode_status?qrcode="+url.QueryEscape(code), session.RouteTag)
	if err != nil {
		return ScanPollResult{}, err
	}
	status, _ := data["status"].(string)
	switch status {
	case "", "wait":
		return ScanPollResult{Status: "pending", Platform: string(ChannelWeixin)}, nil
	case "scaned", "scanned":
		return ScanPollResult{Status: "scanned", Platform: string(ChannelWeixin)}, nil
	case "confirmed":
		token, _ := data["bot_token"].(string)
		if token == "" {
			return ScanPollResult{Status: "error", Error: "确认成功但缺少 bot_token", Platform: string(ChannelWeixin)}, nil
		}
		botID, _ := data["ilink_bot_id"].(string)
		userID, _ := data["ilink_user_id"].(string)
		b.finishScan(code)
		return ScanPollResult{Status: "completed", AppID: botID, AppSecret: token, OwnerOpenID: userID, Platform: string(ChannelWeixin)}, nil
	case "expired":
		return ScanPollResult{Status: "expired", Error: "二维码已过期", Platform: string(ChannelWeixin)}, nil
	default:
		return ScanPollResult{Status: status, Platform: string(ChannelWeixin)}, nil
	}
}

func (b *Bridge) getJSON(ctx context.Context, endpoint, route string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("iLink-App-ClientVersion", "1")
	if route != "" {
		req.Header.Set("SKRouteTag", route)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return data, nil
}
func (b *Bridge) finishScan(code string) { b.mu.Lock(); delete(b.scans, code); b.mu.Unlock() }
func number(value any, fallback float64) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case json.Number:
		v, _ := n.Float64()
		return v
	default:
		return fallback
	}
}
