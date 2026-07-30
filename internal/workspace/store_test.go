package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFolderAndSessions(t *testing.T) {
	root := t.TempDir()
	store, err := NewStoreAt(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root, "project")
	if err := mkdir(folder); err != nil {
		t.Fatal(err)
	}
	w, err := store.OpenFolder(folder)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := store.OpenFolder(folder)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != w2.ID {
		t.Fatalf("opening the same folder created two workspaces: %q != %q", w.ID, w2.ID)
	}
	r, err := store.CreateSession(w.ID, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.ListSessions(w.ID); len(got) != 1 || got[0].ID != r.ID {
		t.Fatalf("unexpected sessions: %#v", got)
	}
	if _, err := NewStoreAt(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}
}

func mkdir(path string) error {
	return os.MkdirAll(path, 0o755)
}
