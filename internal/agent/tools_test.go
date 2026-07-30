package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestToolExecutorOperatesInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	e := &ToolExecutor{Workspace: root}
	if _, err := e.Execute("write_file", `{"path":"src/hello.txt","content":"hello"}`); err != nil {
		t.Fatal(err)
	}
	got, err := e.Execute("read_file", `{"path":"src/hello.txt"}`)
	if err != nil || got != "hello" {
		t.Fatalf("read_file got %q, err=%v", got, err)
	}
	listing, err := e.Execute("list_directory", `{"path":"src"}`)
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(listing), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "hello.txt" {
		t.Fatalf("unexpected listing: %s", listing)
	}
	if _, err := e.Execute("read_file", `{"path":"../outside.txt"}`); err == nil {
		t.Fatal("path traversal was allowed")
	}
	if _, err := os.Stat(filepath.Join(root, "src", "hello.txt")); err != nil {
		t.Fatal(err)
	}
}
