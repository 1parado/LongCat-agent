package i18n

import "testing"

func TestResolve(t *testing.T) {
	cases := map[string]Locale{"zh-CN": LocaleZH, "zh-Hant-TW": LocaleZHTW, "en-US": LocaleEN, "fr-FR": LocaleEN}
	for input, expected := range cases {
		if got := Resolve(input); got != expected {
			t.Fatalf("Resolve(%q)=%q, want %q", input, got, expected)
		}
	}
}
