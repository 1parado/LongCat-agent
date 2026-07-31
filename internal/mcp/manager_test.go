package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestManagerLoadsAndDiscoversTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"tools": []any{map[string]any{"name": "hello", "description": "say hello"}}}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".longcat-frontend"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"servers":[{"id":"demo","name":"Demo","url":"` + srv.URL + `","protocol":"http"}]}`
	if err := os.WriteFile(filepath.Join(root, ".longcat-frontend", "mcp.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	if len(m.Definitions()) != 1 || !m.List()[0].Healthy {
		t.Fatalf("servers=%+v defs=%v", m.List(), m.Definitions())
	}
}
