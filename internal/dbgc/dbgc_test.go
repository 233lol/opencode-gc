package dbgc

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestPlanAndClean(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dbPath := fixture(t, now)
	t.Setenv("OPENCODE_SESSION_ID", "ses_current")

	ro, err := Open(ctx, dbPath, true)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(ctx, ro, PlanOptions{
		Now:          now,
		ProjectAge:   30 * 24 * time.Hour,
		SessionAge:   60 * 24 * time.Hour,
		ArchivedAge:  60 * 24 * time.Hour,
		ReasoningAge: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ro.Close()

	if got := len(plan.Projects); got != 1 || plan.Projects[0].ID != "p_old" {
		t.Fatalf("project candidates = %#v", plan.Projects)
	}
	if got := len(plan.Sessions); got != 1 || plan.Sessions[0].ID != "ses_del" {
		t.Fatalf("session candidates = %#v", plan.Sessions)
	}
	if got := len(plan.Archived); got < 1 || !hasSession(plan.Archived, "ses_arch") {
		t.Fatalf("archived candidates = %#v", plan.Archived)
	}
	if plan.ReasoningCount != 2 {
		t.Fatalf("reasoning count = %d, want 2", plan.ReasoningCount)
	}
	if len(plan.DBCacheKeys) != 2 || !contains(plan.DBCacheKeys, "models-dev:v2:catalog") {
		t.Fatalf("cache keys = %#v", plan.DBCacheKeys)
	}
	if plan.Stats.DBSize == 0 {
		t.Fatal("missing db stats")
	}

	// While the service is running, nothing may change.
	rw, err := Open(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := rw.Clean(ctx, CleanOptions{
		Now:           now,
		DeleteOrphans: true,
		OrphanRules:   plan.Orphans,
		Running:       true,
		AllowRunning:  false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cr.Skipped {
		t.Fatal("clean should be skipped while the service runs")
	}
	rw.Close()

	rw, err = Open(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()

	backupDir := filepath.Join(t.TempDir(), "backups")
	cr, err = rw.Clean(ctx, CleanOptions{
		Now:              now,
		DeleteOrphans:    true,
		OrphanRules:      plan.Orphans,
		Projects:         plan.Projects,
		Sessions:         plan.Sessions,
		Archived:         plan.Archived,
		CompactReasoning: true,
		ReasoningCutoff:  plan.ReasoningCutoff,
		Protected:        plan.Protected,
		DBCacheKeys:      plan.DBCacheKeys,
		Backup:           true,
		BackupDir:        backupDir,
		BackupKeep:       3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cr.Changed {
		t.Fatal("clean should have changed the database")
	}
	if cr.BackupPath == "" {
		t.Fatal("missing backup")
	}
	if info, err := os.Stat(cr.BackupPath); err != nil || info.Size() == 0 {
		t.Fatalf("backup is invalid: %v", err)
	}
	if errs, err := rw.ForeignKeyCheck(ctx, 10); err != nil || len(errs) > 0 {
		t.Fatalf("foreign key check: %v %v", errs, err)
	}

	// Sessions: only the old non-protected ones are gone.
	for _, id := range []string{"ses_del", "ses_arch"} {
		if rowExists(t, rw, `SELECT count(*) FROM session_v2 WHERE id = ?`, id) {
			t.Fatalf("session %s should be deleted", id)
		}
	}
	for _, id := range []string{"ses_keep", "ses_new", "ses_current"} {
		if !rowExists(t, rw, `SELECT count(*) FROM session_v2 WHERE id = ?`, id) {
			t.Fatalf("session %s should be kept", id)
		}
	}
	if rowExists(t, rw, `SELECT count(*) FROM project WHERE id = 'p_old'`) {
		t.Fatal("orphan project should be deleted")
	}

	// Orphan rows.
	for _, q := range []string{
		`SELECT count(*) FROM session_message WHERE id = 'm_orphan'`,
		`SELECT count(*) FROM session_part WHERE message_id = 'm_missing'`,
		`SELECT count(*) FROM blob WHERE hash = 'blob_orphan'`,
		`SELECT count(*) FROM instruction_blob WHERE hash = 'unusedhash'`,
		`SELECT count(*) FROM event WHERE id = 'e_orphan'`,
		`SELECT count(*) FROM event_sequence WHERE aggregate_id = 'missing'`,
		`SELECT count(*) FROM job WHERE notification_id = 'j_orphan'`,
		`SELECT count(*) FROM schedule WHERE id = 'sc_orphan'`,
		`SELECT count(*) FROM session_inbox WHERE id = 'i_orphan'`,
	} {
		if rowExists(t, rw, q) {
			t.Fatalf("orphan row still exists: %s", q)
		}
	}

	// Reasoning is stripped from the kept old session, but not from fresh or
	// protected sessions.
	var keepData string
	if err := rw.SQL().QueryRow(`SELECT data FROM session_message WHERE id = 'm_keep'`).Scan(&keepData); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(keepData, `"reasoning"`) || !strings.Contains(keepData, `"text"`) {
		t.Fatalf("reasoning was not stripped correctly: %s", keepData)
	}
	var newData string
	if err := rw.SQL().QueryRow(`SELECT data FROM session_message WHERE id = 'm_new'`).Scan(&newData); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newData, `"reasoning"`) {
		t.Fatal("recent message reasoning should be kept")
	}
	var currentData string
	if err := rw.SQL().QueryRow(`SELECT data FROM session_message WHERE id = 'm_current'`).Scan(&currentData); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(currentData, `"reasoning"`) {
		t.Fatal("protected session reasoning should be kept")
	}

	// Regenerable cache removed, real settings kept.
	if rowExists(t, rw, `SELECT count(*) FROM kv WHERE key = 'models-dev:v2:catalog'`) {
		t.Fatal("model cache should be deleted")
	}
	if !rowExists(t, rw, `SELECT count(*) FROM kv WHERE key = 'websearch:provider'`) {
		t.Fatal("unrelated kv rows must be kept")
	}

	if err := rw.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rw.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rw.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStripReasoning(t *testing.T) {
	in := `{"content":[{"type":"reasoning","text":"secret"},{"type":"text","text":"hello"}],"x":1}`
	out, changed := stripReasoning(in)
	if !changed {
		t.Fatal("expected a change")
	}
	if strings.Contains(out, "reasoning") || !strings.Contains(out, "hello") || !strings.Contains(out, `"x":1`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if _, changed := stripReasoning(out); changed {
		t.Fatal("stripping must be idempotent")
	}
	if _, changed := stripReasoning(`{"content":[{"type":"text","text":"hi"}]}`); changed {
		t.Fatal("must not change messages without reasoning")
	}
	if _, changed := stripReasoning(`not json`); changed {
		t.Fatal("must not change invalid json")
	}
}

func hasSession(list []SessionCandidate, id string) bool {
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}

func contains(list []string, value string) bool {
	for _, s := range list {
		if s == value {
			return true
		}
	}
	return false
}

func rowExists(t *testing.T, db *DB, query string, args ...any) bool {
	t.Helper()
	var n int
	if err := db.SQL().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n > 0
}

func fixture(t *testing.T, now time.Time) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")

	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	stmts := []string{
		`CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE session_v2 (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT, fork_session_id TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, time_archived INTEGER)`,
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE session_part (message_id TEXT NOT NULL, session_id TEXT NOT NULL, ordinal INTEGER NOT NULL, type TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(message_id, ordinal))`,
		`CREATE TABLE blob (hash TEXT PRIMARY KEY, bytes INTEGER NOT NULL, data BLOB NOT NULL)`,
		`CREATE TABLE blob_ref (session_id TEXT NOT NULL, message_id TEXT NOT NULL, hash TEXT NOT NULL, seq INTEGER NOT NULL, PRIMARY KEY(session_id, message_id, hash))`,
		`CREATE TABLE instruction_blob (hash TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE instruction_state (session_id TEXT PRIMARY KEY, initial_values TEXT NOT NULL, current_values TEXT NOT NULL)`,
		`CREATE TABLE instruction_entry (session_id TEXT NOT NULL, key TEXT NOT NULL, value TEXT, PRIMARY KEY(session_id, key))`,
		`CREATE TABLE event (id TEXT PRIMARY KEY, aggregate_id TEXT NOT NULL, seq INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE event_sequence (aggregate_id TEXT PRIMARY KEY, seq INTEGER NOT NULL)`,
		`CREATE TABLE job (notification_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, child_session_id TEXT, status TEXT NOT NULL)`,
		`CREATE TABLE schedule (id TEXT PRIMARY KEY, session_id TEXT, status TEXT NOT NULL)`,
		`CREATE TABLE session_inbox (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, payload TEXT NOT NULL)`,
		`CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	}
	for _, s := range stmts {
		if _, err := handle.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	day := 24 * time.Hour
	reasoning := `{"content":[{"type":"reasoning","text":"secret"},{"type":"text","text":"hello"}]}`

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := handle.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	exec(`INSERT INTO project VALUES ('p_old','C:/old',?,?)`, ms(60*day), ms(60*day))
	exec(`INSERT INTO project VALUES ('p_live','C:/live',?,?)`, ms(120*day), ms(day))
	exec(`INSERT INTO project VALUES ('p_cur','C:/cur',?,?)`, ms(120*day), ms(day))

	exec(`INSERT INTO session_v2 VALUES ('ses_del','p_live',NULL,NULL,?,?,0)`, ms(120*day), ms(90*day))
	exec(`INSERT INTO session_v2 VALUES ('ses_keep','p_live',NULL,NULL,?,?,0)`, ms(60*day), ms(45*day))
	exec(`INSERT INTO session_v2 VALUES ('ses_arch','p_live',NULL,NULL,?,?,?)`, ms(120*day), ms(45*day), ms(90*day))
	exec(`INSERT INTO session_v2 VALUES ('ses_new','p_live',NULL,NULL,?,?,0)`, ms(time.Hour), ms(time.Hour))
	exec(`INSERT INTO session_v2 VALUES ('ses_current','p_cur',NULL,NULL,?,?,0)`, ms(120*day), ms(90*day))

	exec(`INSERT INTO session_message VALUES ('m_del','ses_del','assistant',?,?)`, ms(100*day), reasoning)
	exec(`INSERT INTO session_message VALUES ('m_keep','ses_keep','assistant',?,?)`, ms(45*day), reasoning)
	exec(`INSERT INTO session_message VALUES ('m_new','ses_new','assistant',?,?)`, ms(time.Hour), reasoning)
	exec(`INSERT INTO session_message VALUES ('m_current','ses_current','assistant',?,?)`, ms(100*day), reasoning)
	exec(`INSERT INTO session_message VALUES ('m_orphan','ses_missing','assistant',?,?)`, ms(day), `{"content":[{"type":"text","text":"orphan"}]}`)

	exec(`INSERT INTO session_part VALUES ('m_missing','ses_missing',0,'text',?, '{}')`, ms(day))
	exec(`INSERT INTO session_part VALUES ('m_keep','ses_keep',0,'reasoning',?, '{"type":"reasoning"}')`, ms(45*day))

	exec(`INSERT INTO blob VALUES ('blob_ref',100,x'00')`)
	exec(`INSERT INTO blob VALUES ('blob_orphan',50,x'00')`)
	exec(`INSERT INTO blob VALUES ('blob_del',10,x'00')`)
	exec(`INSERT INTO blob_ref VALUES ('ses_keep','m_keep','blob_ref',1)`)
	exec(`INSERT INTO blob_ref VALUES ('ses_del','m_del','blob_del',1)`)
	exec(`INSERT INTO blob_ref VALUES ('ses_missing','m_missing','blob_ref',1)`)

	exec(`INSERT INTO instruction_blob VALUES ('usedhash','used')`)
	exec(`INSERT INTO instruction_blob VALUES ('unusedhash','unused')`)
	exec(`INSERT INTO instruction_state VALUES ('ses_keep','{"k":"usedhash"}','{"k":"usedhash"}')`)
	exec(`INSERT INTO instruction_entry VALUES ('ses_missing','k','v')`)

	exec(`INSERT INTO event VALUES ('e_del','ses_del',1,'{}')`)
	exec(`INSERT INTO event VALUES ('e_orphan','missing',1,'{}')`)
	exec(`INSERT INTO event_sequence VALUES ('ses_del',10)`)
	exec(`INSERT INTO event_sequence VALUES ('p_old',0)`)
	exec(`INSERT INTO event_sequence VALUES ('missing',5)`)

	exec(`INSERT INTO job VALUES ('j_del','ses_del',NULL,'completed')`)
	exec(`INSERT INTO job VALUES ('j_orphan','missing',NULL,'completed')`)
	exec(`INSERT INTO job VALUES ('j_child','ses_keep','missing','completed')`)
	exec(`INSERT INTO schedule VALUES ('sc_del','ses_del','active')`)
	exec(`INSERT INTO schedule VALUES ('sc_orphan','missing','active')`)
	exec(`INSERT INTO session_inbox VALUES ('i_orphan','missing','payload')`)

	exec(`INSERT INTO kv VALUES ('models-dev:v2:catalog',?)`, strings.Repeat("x", 100))
	exec(`INSERT INTO kv VALUES ('models-dev:v2:catalog:meta',?)`, strings.Repeat("y", 20))
	exec(`INSERT INTO kv VALUES ('websearch:provider','opencode')`)

	return path
}
