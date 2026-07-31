package i18n

import (
	"encoding/json"
	"os"
	"strings"
)

// Resolve maps BCP-47 tags to the supported catalog set.
func Resolve(language string) Locale {
	language = strings.ToLower(strings.TrimSpace(language))
	if language == "" {
		return LocaleEN
	}
	if strings.HasPrefix(language, "zh-hant") || strings.HasPrefix(language, "zh-tw") || strings.HasPrefix(language, "zh-hk") || strings.HasPrefix(language, "zh-mo") {
		return LocaleZHTW
	}
	if strings.HasPrefix(language, "zh") {
		return LocaleZH
	}
	return LocaleEN
}

// DetectSystem checks the common locale environment variables for CLI use.
func DetectSystem() Locale {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := os.Getenv(key); value != "" {
			return Resolve(value)
		}
	}
	return LocaleEN
}

type Settings struct {
	Locale Preference `json:"locale"`
}

func LoadPreference(path string) (Preference, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PreferenceSystem, nil
		}
		return "", err
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return "", err
	}
	if s.Locale == "" {
		return PreferenceSystem, nil
	}
	return s.Locale, nil
}

func SavePreference(path string, preference Preference) error {
	var s Settings
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	s.Locale = preference
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepathDir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func filepathDir(path string) string {
	i := strings.LastIndexAny(path, `/\\`)
	if i < 0 {
		return "."
	}
	return path[:i]
}
