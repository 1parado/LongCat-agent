package im

import "strings"

// ParseControl parses the provider-neutral command grammar used by all IM
// adapters. Non-command input is intentionally preserved verbatim.
func ParseControl(text string) ControlCommand {
	text = strings.TrimSpace(text)
	if text == "" {
		return ControlCommand{Type: ControlUserMessage}
	}
	if text == "0" || strings.EqualFold(text, "cancel") {
		return ControlCommand{Type: ControlCancel}
	}
	parts := strings.Fields(text)
	command := strings.ToLower(parts[0])
	query := strings.TrimSpace(strings.TrimPrefix(text, parts[0]))
	switch command {
	case "/p":
		if query == "" {
			return ControlCommand{Type: ControlProjectList}
		}
		return ControlCommand{Type: ControlProjectSelect, Query: query}
	case "/r":
		if query == "" {
			return ControlCommand{Type: ControlResumeList}
		}
		return ControlCommand{Type: ControlResumeSelect, Query: query}
	case "/new":
		return ControlCommand{Type: ControlNewSession, Query: query}
	case "/stop":
		return ControlCommand{Type: ControlStop}
	case "/whoami":
		return ControlCommand{Type: ControlWhoami}
	case "/help":
		return ControlCommand{Type: ControlHelp}
	default:
		return ControlCommand{Type: ControlUserMessage, Text: text}
	}
}

func VisibleUserContent(channel RemoteChannelID, text string) string {
	return "[Remote IM · " + string(channel) + "]\n" + strings.TrimSpace(text)
}
