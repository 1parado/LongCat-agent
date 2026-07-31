// Package im contains provider-neutral models for remote messaging adapters.
// Provider transports can be added independently without changing agent
// sessions or the control-plane parser.
package im

type RemoteChannelID string

const (
	ChannelFeishu   RemoteChannelID = "feishu"
	ChannelLark     RemoteChannelID = "lark"
	ChannelDingTalk RemoteChannelID = "dingtalk"
	ChannelWeCom    RemoteChannelID = "wecom"
	ChannelWeixin   RemoteChannelID = "weixin"
	ChannelTelegram RemoteChannelID = "telegram"
	ChannelSlack    RemoteChannelID = "slack"
	ChannelDiscord  RemoteChannelID = "discord"
	ChannelMatrix   RemoteChannelID = "matrix"
)

type StatusTone string

const (
	StatusConnected    StatusTone = "connected"
	StatusConfigured   StatusTone = "configured"
	StatusUnconfigured StatusTone = "unconfigured"
	StatusError        StatusTone = "error"
)

type ProjectScope string

const (
	ScopeAnyProject ProjectScope = "any"
	ScopeAllowList  ProjectScope = "allow_list"
	ScopeCurrent    ProjectScope = "current"
)

type PresenterMode string

const (
	PresenterOff PresenterMode = "off"
	PresenterOn  PresenterMode = "on"
)

type ACLConfig struct {
	AllowFrom      string `json:"allow_from,omitempty"`
	AllowChat      string `json:"allow_chat,omitempty"`
	RequireMention bool   `json:"require_mention,omitempty"`
	GroupOnly      bool   `json:"group_only,omitempty"`
	AdminFrom      string `json:"admin_from,omitempty"`
	ShareSession   bool   `json:"share_session,omitempty"`
}

type ChannelInstance struct {
	ID             string          `json:"id"`
	Channel        RemoteChannelID `json:"channel"`
	Name           string          `json:"name"`
	Enabled        bool            `json:"enabled"`
	Credentials    string          `json:"credentials_ref,omitempty"`
	Options        map[string]any  `json:"options,omitempty"`
	ACL            ACLConfig       `json:"acl"`
	ProjectScope   ProjectScope    `json:"project_scope"`
	Presenter      PresenterMode   `json:"presenter"`
	Status         StatusTone      `json:"status"`
	HasCredentials bool            `json:"has_credentials"`
	LastError      string          `json:"last_error,omitempty"`
}

type BridgeStatus struct {
	State             string            `json:"state"`
	Enabled           bool              `json:"enabled"`
	Lifecycle         string            `json:"lifecycle"`
	ConnectedChannels []ChannelInstance `json:"connected_channels"`
	LastError         string            `json:"last_error,omitempty"`
	Backend           string            `json:"backend"`
}

type ScanBeginResult struct {
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	IntervalSec     int    `json:"interval_sec"`
	ExpireInSec     int    `json:"expire_in_sec"`
	Platform        string `json:"platform"`
}

type ScanPollResult struct {
	Status          string `json:"status"`
	AppID           string `json:"app_id,omitempty"`
	AppSecret       string `json:"app_secret,omitempty"`
	OwnerOpenID     string `json:"owner_open_id,omitempty"`
	Platform        string `json:"platform,omitempty"`
	Error           string `json:"error,omitempty"`
	VerificationURI string `json:"verification_uri,omitempty"`
	DeviceCode      string `json:"device_code,omitempty"`
}

type ControlType string

const (
	ControlProjectList   ControlType = "project_list"
	ControlProjectSelect ControlType = "project_select"
	ControlResumeList    ControlType = "resume_list"
	ControlResumeSelect  ControlType = "resume_select"
	ControlNewSession    ControlType = "new_session"
	ControlStatus        ControlType = "status"
	ControlWhoami        ControlType = "whoami"
	ControlStop          ControlType = "stop"
	ControlHelp          ControlType = "help"
	ControlCancel        ControlType = "cancel"
	ControlUserMessage   ControlType = "user_message"
)

type ControlCommand struct {
	Type  ControlType `json:"type"`
	Query string      `json:"query,omitempty"`
	Text  string      `json:"text,omitempty"`
}
type IncomingMessage struct {
	Channel   RemoteChannelID `json:"channel"`
	UserID    string          `json:"user_id"`
	ChatID    string          `json:"chat_id"`
	Text      string          `json:"text"`
	Mentioned bool            `json:"mentioned"`
}
