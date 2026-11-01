package dbgc

import (
	"context"
	"testing"
	"time"
)

func TestLoadSessionStats(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dbPath := fixture(t, now)
	t.Setenv("OPENCODE_SESSION_ID", "ses_current")

	db, err := Open(ctx, dbPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stats, err := LoadSessionStats(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	keep := stats["ses_keep"]
	if keep.Messages != 1 || keep.MessageBytes <= 0 || keep.TotalBytes < keep.MessageBytes {
		t.Fatalf("ses_keep stats = %#v", keep)
	}
	del := stats["ses_del"]
	if del.Messages != 1 || del.TotalBytes == 0 {
		t.Fatalf("ses_del stats = %#v", del)
	}
	if _, ok := stats["ses_new"]; !ok {
		t.Fatal("missing ses_new stats")
	}
}

func TestCleanForceBypasses24h(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dbPath := fixture(t, now)
	t.Setenv("OPENCODE_SESSION_ID", "ses_current")

	db, err := Open(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	meta, err := LoadMeta(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := LoadSessionStats(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	s := meta.Sessions["ses_new"]
	cand := []SessionCandidate{{
		Session:  s,
		Messages: stats[s.ID].Messages,
		Bytes:    stats[s.ID].MessageBytes,
	}}
	base := CleanOptions{
		Now:           now,
		DeleteOrphans: true,
		OrphanRules:   OrphanRules(db),
		Sessions:      cand,
		Protected:     meta.Protected,
		Running:       false,
		Backup:        false,
	}

	if _, err := db.Clean(ctx, base); err != nil {
		t.Fatal(err)
	}
	if !rowExists(t, db, `SELECT count(*) FROM session_v2 WHERE id = 'ses_new'`) {
		t.Fatal("a recent session must not be deleted without --force")
	}

	base.Force = true
	if _, err := db.Clean(ctx, base); err != nil {
		t.Fatal(err)
	}
	if rowExists(t, db, `SELECT count(*) FROM session_v2 WHERE id = 'ses_new'`) {
		t.Fatal("--force should allow deleting a recent session")
	}
}
