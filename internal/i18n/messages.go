// Package i18n contains the shared locale model used by CLI/API clients.
package i18n

import (
	"strings"
)

type Locale string

const (
	LocaleZH   Locale = "zh"
	LocaleZHTW Locale = "zh-TW"
	LocaleEN   Locale = "en"
)

type Preference string

const (
	PreferenceSystem Preference = "system"
)

type MessageKey string

const (
	MsgConnected         MessageKey = "connected"
	MsgDisconnected      MessageKey = "disconnected"
	MsgWorkspaceRequired MessageKey = "workspace_required"
	MsgNoUndo            MessageKey = "no_undo"
	MsgUndoDone          MessageKey = "undo_done"
	MsgPreviewPath       MessageKey = "preview_path"
)

var catalog = map[Locale]map[MessageKey]string{
	LocaleZH:   {MsgConnected: "已连接", MsgDisconnected: "未连接", MsgWorkspaceRequired: "请先打开一个文件夹", MsgNoUndo: "没有可撤销的文件修改", MsgUndoDone: "已撤销 {file}", MsgPreviewPath: "预览路径必须位于当前文件夹内"},
	LocaleZHTW: {MsgConnected: "已連線", MsgDisconnected: "未連線", MsgWorkspaceRequired: "請先開啟一個資料夾", MsgNoUndo: "沒有可復原的檔案修改", MsgUndoDone: "已復原 {file}", MsgPreviewPath: "預覽路徑必須位於目前資料夾內"},
	LocaleEN:   {MsgConnected: "Connected", MsgDisconnected: "Disconnected", MsgWorkspaceRequired: "Open a folder first", MsgNoUndo: "There are no file changes to undo", MsgUndoDone: "Undid {file}", MsgPreviewPath: "Preview path must stay inside the current folder"},
}

func Normalize(locale Locale) Locale {
	switch locale {
	case LocaleZH, LocaleZHTW, LocaleEN:
		return locale
	default:
		return LocaleEN
	}
}

func T(locale Locale, key MessageKey, vars map[string]string) string {
	locale = Normalize(locale)
	text := catalog[locale][key]
	if text == "" {
		text = catalog[LocaleEN][key]
	}
	if text == "" {
		text = string(key)
	}
	for key, value := range vars {
		text = strings.ReplaceAll(text, "{"+key+"}", value)
	}
	return text
}
