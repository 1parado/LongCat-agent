package workspace

import (
	"fmt"
	"os"
	"strings"
)

type Diff struct {
	Path    string     `json:"path"`
	Kind    ChangeKind `json:"kind"`
	Before  string     `json:"before"`
	After   string     `json:"after"`
	Unified string     `json:"unified"`
}

// UnifiedDiff creates a compact line-oriented diff without external tools.
func UnifiedDiff(path, before, after string) string {
	oldLines := splitLines(before)
	newLines := splitLines(after)
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	if before == after {
		return b.String()
	}
	// A simple one-hunk representation is predictable for review and works for
	// both new files and edits. The UI can still show the exact before/after.
	fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", len(oldLines), len(newLines))
	for _, line := range oldLines {
		b.WriteString("-")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, line := range newLines {
		b.WriteString("+")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "\n")
}

func fileContent(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
