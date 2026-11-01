package dbgc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneBackupsOnlyRemovesAutomaticBackups(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	newer := now.Add(-1 * time.Hour)

	write := func(name string, mod time.Time) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
		return path
	}

	write("autobackup-old.db", old)
	write("autobackup-new.db", newer)
	manual := write("opencode-manual.db", old)
	write("unrelated.txt", old)

	removed := PruneBackups(dir, 1)
	if len(removed) != 1 || filepath.Base(removed[0]) != "autobackup-old.db" {
		t.Fatalf("removed = %#v", removed)
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatal("manual backup must not be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated.txt")); err != nil {
		t.Fatal("unrelated file must not be pruned")
	}
}
