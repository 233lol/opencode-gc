// Package dbgc scans and cleans the OpenCode SQLite database.
package dbgc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrUnsupported means the file is not a known OpenCode V2 database.
var ErrUnsupported = errors.New("unsupported database schema (expected OpenCode V2 session_v2 table)")

// DB wraps the SQLite handle and caches the table list.
type DB struct {
	sql      *sql.DB
	Path     string
	ReadOnly bool
	Tables   map[string]bool
}

// Open opens the database at path. A missing file returns os.ErrNotExist.
func Open(ctx context.Context, path string, readOnly bool) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}

	dsn := filepath.ToSlash(abs)

	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	handle.SetConnMaxLifetime(0)

	d := &DB{sql: handle, Path: abs, ReadOnly: readOnly, Tables: make(map[string]bool)}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := handle.PingContext(pingCtx); err != nil {
		handle.Close()
		return nil, err
	}

	pragmas := []string{
		"PRAGMA busy_timeout=10000",
	}
	if readOnly {
		pragmas = append(pragmas, "PRAGMA query_only=ON")
	} else {
		pragmas = append(pragmas, "PRAGMA foreign_keys=ON")
	}
	for _, p := range pragmas {
		if _, err := handle.ExecContext(ctx, p); err != nil {
			handle.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	if err := d.loadTables(ctx); err != nil {
		handle.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// SQL exposes the underlying handle for read-only helper queries.
func (d *DB) SQL() *sql.DB { return d.sql }

func (d *DB) loadTables(ctx context.Context) error {
	rows, err := d.sql.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		d.Tables[name] = true
	}
	return rows.Err()
}

// HasTable reports whether a table exists.
func (d *DB) HasTable(name string) bool {
	_, ok := d.Tables[name]
	return ok
}

// CheckV2 verifies that the core V2 tables and columns are present.
func (d *DB) CheckV2(ctx context.Context) error {
	required := []string{"session_v2", "project", "session_message"}
	for _, name := range required {
		if !d.HasTable(name) {
			return ErrUnsupported
		}
	}
	requiredCols := []string{"id", "project_id", "parent_id", "fork_session_id", "time_created", "time_updated", "time_archived"}
	rows, err := d.sql.QueryContext(ctx, `PRAGMA table_info(session_v2)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	have := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range requiredCols {
		if !have[name] {
			return fmt.Errorf("%w: missing session_v2.%s", ErrUnsupported, name)
		}
	}
	return nil
}

// Stats holds file and page statistics.
type Stats struct {
	DBSize    int64
	WALSize   int64
	SHMSize   int64
	PageSize  int64
	PageCount int64
	Freelist  int64
	FreeBytes int64
}

// GetStats collects database size information.
func (d *DB) GetStats(ctx context.Context) Stats {
	var s Stats
	if info, err := os.Stat(d.Path); err == nil {
		s.DBSize = info.Size()
	}
	if info, err := os.Stat(d.Path + "-wal"); err == nil {
		s.WALSize = info.Size()
	}
	if info, err := os.Stat(d.Path + "-shm"); err == nil {
		s.SHMSize = info.Size()
	}
	_ = d.sql.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&s.PageSize)
	_ = d.sql.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&s.PageCount)
	_ = d.sql.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&s.Freelist)
	if s.PageSize > 0 && s.Freelist > 0 {
		s.FreeBytes = s.PageSize * s.Freelist
	}
	return s
}

// QuickCheck runs SQLite's quick integrity check.
func (d *DB) QuickCheck(ctx context.Context) error {
	var result string
	if err := d.sql.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return err
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("quick_check: %s", result)
	}
	return nil
}

// ForeignKeyCheck returns up to limit violations.
func (d *DB) ForeignKeyCheck(ctx context.Context, limit int) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return out, err
		}
		out = append(out, fmt.Sprintf("%s rowid=%v parent=%s fk=%v", table, rowid.Int64, parent, fkid.Int64))
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// Backup creates a consistent copy of the database with VACUUM INTO.
func (d *DB) Backup(ctx context.Context, dest string) (int64, error) {
	dest, err := filepath.Abs(dest)
	if err != nil {
		return 0, err
	}
	if _, err := os.Stat(dest); err == nil {
		return 0, fmt.Errorf("backup destination already exists: %s", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}

	quoted := strings.ReplaceAll(filepath.ToSlash(dest), "'", "''")
	stmt := fmt.Sprintf("VACUUM INTO '%s'", quoted)
	if _, err := d.sql.ExecContext(ctx, stmt); err != nil {
		return 0, fmt.Errorf("VACUUM INTO backup: %w", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}

	// Verify the copy before we touch the original.
	check, err := Open(ctx, dest, true)
	if err != nil {
		return info.Size(), fmt.Errorf("verify backup: %w", err)
	}
	defer check.Close()
	if err := check.QuickCheck(ctx); err != nil {
		return info.Size(), fmt.Errorf("verify backup: %w", err)
	}
	return info.Size(), nil
}

// BackupName builds a timestamped manual backup file name.
func BackupName(now time.Time) string {
	return backupName("opencode-", now)
}

// AutoBackupName builds a timestamped automatic backup file name. Only files
// with this prefix are pruned by the retention policy.
func AutoBackupName(now time.Time) string {
	return backupName("autobackup-", now)
}

func backupName(prefix string, now time.Time) string {
	return prefix + now.Format("20060102-150405") + ".db"
}

// PruneBackups keeps the newest keep automatic backups.
func PruneBackups(dir string, keep int) []string {
	if keep <= 0 {
		keep = 3
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type item struct {
		path string
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "autobackup-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: filepath.Join(dir, name), mod: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	var removed []string
	for i := keep; i < len(items); i++ {
		if err := os.Remove(items[i].path); err == nil {
			removed = append(removed, items[i].path)
		}
	}
	return removed
}

// Checkpoint truncates the WAL. It returns an error if another connection is
// still using the database.
func (d *DB) Checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := d.sql.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("WAL checkpoint is busy; stop the OpenCode service first")
	}
	return nil
}

// Vacuum checkpoints, vacuums and optimizes the database.
func (d *DB) Vacuum(ctx context.Context) error {
	if err := d.Checkpoint(ctx); err != nil {
		return err
	}
	if _, err := d.sql.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	if _, err := d.sql.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return err
	}
	// VACUUM writes through the WAL; truncate it again so the reported size
	// reflects the reclaimed space.
	_ = d.Checkpoint(ctx)
	return nil
}
