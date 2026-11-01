package fsgc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"opencode-gc/internal/report"
)

func writeFile(t *testing.T, path, data string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestScanFlatAge(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opencode")
	now := time.Now()
	writeFile(t, filepath.Join(root, "old.txt"), "old", now.Add(-10*24*time.Hour))
	writeFile(t, filepath.Join(root, "new.txt"), "new", now.Add(-1*time.Hour))
	writeFile(t, filepath.Join(root, "sub", "deep.txt"), "deep", now.Add(-9*24*time.Hour))

	res := Scan(Options{
		TmpDir: root,
		Age:    7 * 24 * time.Hour,
		Now:    now,
	})
	var tmp *report.Finding
	for i := range res.Findings {
		if res.Findings[i].Category == "tmp" {
			tmp = &res.Findings[i]
		}
	}
	if tmp == nil {
		t.Fatal("missing tmp finding")
	}
	if tmp.Count != 2 {
		t.Fatalf("count = %d, want 2", tmp.Count)
	}
	if len(res.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(res.Targets))
	}

	dr := Delete(res.Targets, now.Add(-7*24*time.Hour))
	if dr.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (%v)", dr.Deleted, dr.Errors)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal("recent file should be kept")
	}
	if _, err := os.Stat(filepath.Join(root, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("old file should be deleted")
	}
}

func TestReferencedShellOutputKept(t *testing.T) {
	data := t.TempDir()
	shell := filepath.Join(data, "shell")
	project := "proj1"
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	writeFile(t, filepath.Join(shell, project, "sh_keep.out"), "keep", old)
	writeFile(t, filepath.Join(shell, project, "sh_drop.out"), "drop", old)

	refs := NewRefIndex()
	refs.Add("sh_keep.out")
	res := Scan(Options{
		ShellDir:     shell,
		LiveProjects: map[string]struct{}{project: {}},
		Refs:         refs,
		Age:          7 * 24 * time.Hour,
		Now:          now,
	})
	if len(res.Targets) != 1 {
		t.Fatalf("targets = %d, want 1: %#v", len(res.Targets), res.Targets)
	}
	if filepath.Base(res.Targets[0].Path) != "sh_drop.out" {
		t.Fatalf("wrong target: %s", res.Targets[0].Path)
	}
	dr := Delete(res.Targets, now.Add(-7*24*time.Hour))
	if dr.Deleted != 1 {
		t.Fatalf("deleted = %d: %v", dr.Deleted, dr.Errors)
	}
	if _, err := os.Stat(filepath.Join(shell, project, "sh_keep.out")); err != nil {
		t.Fatal("referenced output should be kept")
	}
	if _, err := os.Stat(filepath.Join(shell, project, "sh_drop.out")); !os.IsNotExist(err) {
		t.Fatal("unreferenced output should be deleted")
	}
}

func TestFlatToolOutputKeepsReferenced(t *testing.T) {
	data := t.TempDir()
	tool := filepath.Join(data, "tool-output")
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	writeFile(t, filepath.Join(tool, "tool_aaaaaaaaaaaa.txt"), "keep", old)
	writeFile(t, filepath.Join(tool, "tool_bbbbbbbbbbbb.txt"), "drop", old)

	refs := NewRefIndex()
	refs.Add("tool_aaaaaaaaaaaa.txt")
	res := Scan(Options{
		ToolOutputDir: tool,
		LiveSessions:  map[string]struct{}{"ses_x": {}},
		Refs:          refs,
		Age:           7 * 24 * time.Hour,
		Now:           now,
	})
	if len(res.Targets) != 1 {
		t.Fatalf("targets = %d, want 1: %#v", len(res.Targets), res.Targets)
	}
	if filepath.Base(res.Targets[0].Path) != "tool_bbbbbbbbbbbb.txt" {
		t.Fatalf("wrong target: %s", res.Targets[0].Path)
	}
}

func TestDeleteRefusesOutsideRoot(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "important.txt")
	writeFile(t, outside, "keep", time.Now().Add(-100*time.Hour))
	root := filepath.Join(t.TempDir(), "root")
	dr := Delete([]Target{{Root: root, Path: outside, Count: 1, Bytes: 5}}, time.Now())
	if dr.Deleted != 0 {
		t.Fatal("must not delete outside root")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("outside file was deleted")
	}
}
