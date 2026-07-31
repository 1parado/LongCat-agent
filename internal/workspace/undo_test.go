package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUndoStoreRestoresCreateAndOverwrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := NewUndoStoreAt(filepath.Join(t.TempDir(), "undo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := u.Record(root, "notes.txt", []byte("before"), true, "agent write_file"); err != nil {
		t.Fatal(err)
	}
	if _, err := u.UndoLast(root); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "before" {
		t.Fatalf("restored %q", b)
	}
	if err := u.Record(root, "new.txt", nil, false, "agent write_file"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := u.UndoLast(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new file still exists: %v", err)
	}
}
