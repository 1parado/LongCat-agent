package im

import "testing"

func TestParseControl(t *testing.T) {
	cases := map[string]ControlType{"/p": ControlProjectList, "/p demo": ControlProjectSelect, "/r 123": ControlResumeSelect, "/new": ControlNewSession, "/stop": ControlStop, "/whoami": ControlWhoami, "cancel": ControlCancel, "hello": ControlUserMessage}
	for input, expected := range cases {
		if got := ParseControl(input); got.Type != expected {
			t.Fatalf("ParseControl(%q)=%q, want %q", input, got.Type, expected)
		}
	}
}
