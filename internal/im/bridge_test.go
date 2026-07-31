package im

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBridgePersistsInstancesWithoutSecretsInConfig(t *testing.T) {
	dir := t.TempDir()
	b, err := NewBridgeAt(filepath.Join(dir, "im.json"), filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := b.SaveInstance(ChannelInstance{Channel: ChannelFeishu, Name: "Work Feishu", Enabled: true}, map[string]string{"app_secret": "top-secret"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !instance.HasCredentials || instance.Credentials != "" {
		t.Fatalf("public instance leaked credential state: %+v", instance)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "im.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "top-secret") {
		t.Fatal("secret was written to the public config")
	}
	reloaded, err := NewBridgeAt(filepath.Join(dir, "im.json"), filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	list := reloaded.ListInstances()
	if len(list) != 1 || !list[0].HasCredentials {
		t.Fatalf("reloaded instances=%+v", list)
	}
}

func TestQRDataURL(t *testing.T) {
	value, err := qrDataURL("https://example.com/login?device=abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(value, "data:image/png;base64,") {
		t.Fatalf("unexpected QR data URL: %q", value[:min(len(value), 32)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
