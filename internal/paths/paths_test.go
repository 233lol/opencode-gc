package paths

import (
	"path/filepath"
	"testing"
)

func TestApplyDataDir(t *testing.T) {
	dataDir := t.TempDir()
	p := Paths{Data: dataDir, Tmp: dataDir, DB: filepath.Join(dataDir, "opencode.db"), Log: filepath.Join(dataDir, "log")}
	p.Apply(dataDir, "", "", "")
	if p.Data != filepath.Clean(dataDir) {
		t.Fatalf("data = %s", p.Data)
	}
	if p.Log != filepath.Join(p.Data, "log") {
		t.Fatalf("log = %s", p.Log)
	}
	if p.Shell != filepath.Join(p.Data, "shell") {
		t.Fatalf("shell = %s", p.Shell)
	}
	if p.DB != filepath.Join(p.Data, "opencode.db") {
		t.Fatalf("db = %s", p.DB)
	}
}

func TestApplyDBOverrideMovesBackup(t *testing.T) {
	p := Paths{Data: `C:\data`, Tmp: `C:\tmp`, DB: `C:\data\opencode.db`, BackupDir: `C:\data\backups`}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	p.Apply("", "", copyPath, "")
	if p.DB == "" || filepath.Base(p.DB) != "copy.db" {
		t.Fatalf("db = %s", p.DB)
	}
	if p.BackupDir != filepath.Join(filepath.Dir(p.DB), "backups") {
		t.Fatalf("backup dir = %s", p.BackupDir)
	}
}
