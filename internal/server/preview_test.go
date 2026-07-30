package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/workspace"
)

func TestPreviewFileServesActiveWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>ok</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := workspace.NewStoreAt(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.OpenFolder(root)
	if err != nil {
		t.Fatal(err)
	}
	a := &api{session: &agent.Session{}, workspaces: store, activeWorkspace: w}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/preview-file", a.previewFile)
	mux.HandleFunc("GET /api/preview/{path...}", a.previewFile)

	req := httptest.NewRequest(http.MethodGet, "/api/preview/index.html", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h1>ok</h1>") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}

	outside := filepath.Join(filepath.Dir(root), "outside.html")
	_ = os.WriteFile(outside, []byte("secret"), 0o644)
	req = httptest.NewRequest(http.MethodGet, "/api/preview/../outside.html", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("preview allowed a path outside the active workspace")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/preview-file?path="+url.QueryEscape(filepath.Join(root, "index.html")), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h1>ok</h1>") {
		t.Fatalf("absolute path status=%d body=%q", rec.Code, rec.Body.String())
	}
}
