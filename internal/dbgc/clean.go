package dbgc

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanOptions describes the database mutations to perform.
type CleanOptions struct {
	Now time.Time

	DeleteOrphans bool
	OrphanRules   []OrphanRule

	Projects []ProjectCandidate
	Sessions []SessionCandidate
	Archived []SessionCandidate

	CompactReasoning bool
	ReasoningCutoff  int64
	Protected        map[string]bool

	DBCacheKeys []string

	Vacuum     bool
	Backup     bool
	BackupDir  string
	BackupKeep int

	Running      bool
	AllowRunning bool
}

// CleanResult reports what happened.
type CleanResult struct {
	Rows        int64
	Changed     bool
	Skipped     bool
	BackupPath  string
	BackupBytes int64
	VacuumDone  bool
	Warnings    []string
	Errors      []string
}

// Clean applies the plan. If the OpenCode service is running, database writes
// are skipped unless AllowRunning is set; VACUUM is never attempted while it is
// running.
func (d *DB) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var res CleanResult

	dbNeeded := opts.DeleteOrphans ||
		len(opts.Projects) > 0 ||
		len(opts.Sessions) > 0 ||
		len(opts.Archived) > 0 ||
		opts.CompactReasoning ||
		len(opts.DBCacheKeys) > 0

	if !dbNeeded && !opts.Vacuum {
		return res, nil
	}

	if opts.Running && !opts.AllowRunning {
		res.Skipped = true
		res.Warnings = append(res.Warnings,
			"OpenCode 服务正在运行，已跳过数据库清理。请退出 OpenCode 后重试，或先执行 `opencode service stop`。")
		if opts.Vacuum {
			res.Warnings = append(res.Warnings, "服务运行期间不会执行 VACUUM。")
		}
		return res, nil
	}

	vacuum := opts.Vacuum && !opts.Running
	if opts.Vacuum && opts.Running {
		res.Skipped = true
		res.Warnings = append(res.Warnings, "服务运行期间已跳过 VACUUM；请在服务停止后执行 `opencode-gc vacuum`。")
	}

	if dbNeeded {
		if opts.Backup {
			dir := opts.BackupDir
			if dir == "" {
				dir = filepath.Join(filepath.Dir(d.Path), "backups")
			}
			dest := filepath.Join(dir, AutoBackupName(now))
			size, err := d.Backup(ctx, dest)
			if err != nil {
				return res, fmt.Errorf("数据库备份失败，已中止清理: %w", err)
			}
			res.BackupPath = dest
			res.BackupBytes = size
			if removed := PruneBackups(dir, opts.BackupKeep); len(removed) > 0 {
				res.Warnings = append(res.Warnings, fmt.Sprintf("已删除 %d 个旧备份", len(removed)))
			}
		}

		rows, err := d.applyMutations(ctx, opts, now)
		if err != nil {
			return res, err
		}
		res.Rows = rows
		res.Changed = rows > 0

		if violations, err := d.ForeignKeyCheck(ctx, 10); err == nil && len(violations) > 0 {
			res.Errors = append(res.Errors, "清理后仍有外键异常: "+strings.Join(violations, "; "))
		}
	}

	if vacuum {
		if err := d.Vacuum(ctx); err != nil {
			res.Warnings = append(res.Warnings, "VACUUM 失败: "+err.Error())
		} else {
			res.VacuumDone = true
		}
	}

	return res, nil
}

func (d *DB) applyMutations(ctx context.Context, opts CleanOptions, now time.Time) (int64, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var total int64

	sessionIDs := make([]string, 0, len(opts.Sessions)+len(opts.Archived))
	seen := make(map[string]bool)
	for _, list := range [][]SessionCandidate{opts.Sessions, opts.Archived} {
		for _, s := range list {
			if seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			sessionIDs = append(sessionIDs, s.ID)
		}
	}
	if len(sessionIDs) > 0 {
		safe, err := safeSessionIDs(ctx, tx, sessionIDs, opts.Protected, now)
		if err != nil {
			return total, err
		}
		sessionIDs = safe
	}

	if len(sessionIDs) > 0 {
		n, err := execIDBatch(ctx, tx, `DELETE FROM session_v2 WHERE id IN (%s)`, sessionIDs, 200)
		if err != nil {
			return total, err
		}
		total += n

		if d.HasTable("event_sequence") {
			n, err = execIDBatch(ctx, tx, `DELETE FROM event_sequence WHERE aggregate_id IN (%s)`, sessionIDs, 200)
			if err != nil {
				return total, err
			}
			total += n
		}
		if d.HasTable("event") {
			n, err = execIDBatch(ctx, tx, `DELETE FROM event WHERE aggregate_id IN (%s)`, sessionIDs, 200)
			if err != nil {
				return total, err
			}
			total += n
		}
		if d.HasTable("job") {
			for _, batch := range chunks(sessionIDs, 150) {
				ph := placeholders(len(batch))
				args := make([]any, 0, len(batch)*2)
				for _, id := range batch {
					args = append(args, id)
				}
				for _, id := range batch {
					args = append(args, id)
				}
				r, err := tx.ExecContext(ctx, `DELETE FROM job WHERE session_id IN (`+ph+`) OR child_session_id IN (`+ph+`)`, args...)
				if err != nil {
					return total, err
				}
				c, _ := r.RowsAffected()
				total += c
			}
		}
		// Surviving children become top-level sessions.
		n, err = execIDBatch(ctx, tx, `UPDATE session_v2 SET parent_id = NULL WHERE parent_id IN (%s)`, sessionIDs, 200)
		if err != nil {
			return total, err
		}
		total += n
		n, err = execIDBatch(ctx, tx, `UPDATE session_v2 SET fork_session_id = NULL WHERE fork_session_id IN (%s)`, sessionIDs, 200)
		if err != nil {
			return total, err
		}
		total += n
	}

	if len(opts.Projects) > 0 {
		projectIDs := make([]string, 0, len(opts.Projects))
		for _, p := range opts.Projects {
			projectIDs = append(projectIDs, p.ID)
		}
		projectIDs, err := safeProjectIDs(ctx, tx, projectIDs)
		if err != nil {
			return total, err
		}
		if len(projectIDs) > 0 {
			n, err := execIDBatch(ctx, tx, `DELETE FROM project WHERE id IN (%s)`, projectIDs, 200)
			if err != nil {
				return total, err
			}
			total += n
			if d.HasTable("event_sequence") {
				n, err = execIDBatch(ctx, tx, `DELETE FROM event_sequence WHERE aggregate_id IN (%s)`, projectIDs, 200)
				if err != nil {
					return total, err
				}
				total += n
			}
			if d.HasTable("event") {
				n, err = execIDBatch(ctx, tx, `DELETE FROM event WHERE aggregate_id IN (%s)`, projectIDs, 200)
				if err != nil {
					return total, err
				}
				total += n
			}
		}
	}

	// Once anything was removed, always sweep referential leftovers. They are
	// unreachable data either way.
	if opts.DeleteOrphans || len(sessionIDs) > 0 || len(opts.Projects) > 0 {
		for _, rule := range opts.OrphanRules {
			if rule.DeleteSQL == "" {
				continue
			}
			r, err := tx.ExecContext(ctx, rule.DeleteSQL)
			if err != nil {
				return total, fmt.Errorf("%s: %w", rule.Name, err)
			}
			c, _ := r.RowsAffected()
			total += c
		}
	}

	if opts.CompactReasoning {
		rows, _, err := compactReasoning(ctx, tx, d, opts.ReasoningCutoff, opts.Protected)
		if err != nil {
			return total, err
		}
		total += rows
	}

	if len(opts.DBCacheKeys) > 0 && d.HasTable("kv") {
		for _, key := range opts.DBCacheKeys {
			r, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
			if err != nil {
				return total, err
			}
			c, _ := r.RowsAffected()
			total += c
		}
	}

	if err := tx.Commit(); err != nil {
		return total, err
	}
	return total, nil
}

// safeSessionIDs re-checks the hard 24h floor and protects the running
// session, guarding against races between scan and clean.
func safeSessionIDs(ctx context.Context, tx *sql.Tx, ids []string, protected map[string]bool, now time.Time) ([]string, error) {
	current := strings.TrimSpace(os.Getenv("OPENCODE_SESSION_ID"))
	cutoff := now.Add(-24 * time.Hour).UnixMilli()
	var out []string
	for _, batch := range chunks(ids, 200) {
		ph := placeholders(len(batch))
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, time_updated, coalesce(time_archived,0) FROM session_v2 WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var updated, archived int64
			if err := rows.Scan(&id, &updated, &archived); err != nil {
				rows.Close()
				return nil, err
			}
			if protected[id] || id == current {
				continue
			}
			if updated < cutoff || (archived != 0 && archived < cutoff) {
				out = append(out, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// safeProjectIDs re-checks that the projects still have no sessions and are
// not the current working directory, guarding against races.
func safeProjectIDs(ctx context.Context, tx *sql.Tx, ids []string) ([]string, error) {
	wd, _ := os.Getwd()
	var out []string
	for _, batch := range chunks(ids, 200) {
		ph := placeholders(len(batch))
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT p.id, p.worktree FROM project p
			 WHERE p.id IN (`+ph+`)
			   AND NOT EXISTS (SELECT 1 FROM session_v2 s WHERE s.project_id = p.id)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, worktree string
			if err := rows.Scan(&id, &worktree); err != nil {
				rows.Close()
				return nil, err
			}
			if wd != "" && samePath(worktree, wd) {
				continue
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func execIDBatch(ctx context.Context, tx *sql.Tx, format string, ids []string, batchSize int) (int64, error) {
	var total int64
	for _, batch := range chunks(ids, batchSize) {
		ph := placeholders(len(batch))
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		r, err := tx.ExecContext(ctx, fmt.Sprintf(format, ph), args...)
		if err != nil {
			return total, err
		}
		c, _ := r.RowsAffected()
		total += c
	}
	return total, nil
}

func chunks(ids []string, size int) [][]string {
	if size <= 0 {
		size = 200
	}
	var out [][]string
	for i := 0; i < len(ids); i += size {
		end := i + size
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[i:end])
	}
	return out
}

// compactReasoning removes reasoning content entries from old assistant
// messages. It returns the number of updated messages and the number of bytes
// removed.
func compactReasoning(ctx context.Context, tx *sql.Tx, d *DB, cutoff int64, protected map[string]bool) (int64, int64, error) {
	if cutoff <= 0 {
		return 0, 0, nil
	}
	if !d.HasTable("session_message") || !d.HasTable("session_v2") {
		return 0, 0, nil
	}

	protectedIDs := make([]string, 0, len(protected))
	for id := range protected {
		protectedIDs = append(protectedIDs, id)
	}

	q := `SELECT m.id FROM session_message m
	      JOIN session_v2 s ON s.id = m.session_id
	      WHERE m.type='assistant' AND m.time_created < ? AND s.time_updated < ?
	        AND m.data LIKE '%"type":"reasoning"%'`
	args := []any{cutoff, cutoff}
	if len(protectedIDs) > 0 {
		q += ` AND m.session_id NOT IN (` + placeholders(len(protectedIDs)) + `)`
		for _, id := range protectedIDs {
			args = append(args, id)
		}
	}

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()

	var updated, removedBytes int64
	type update struct {
		id   string
		data string
	}
	for _, batch := range chunks(ids, 50) {
		ph := placeholders(len(batch))
		bargs := make([]any, 0, len(batch))
		for _, id := range batch {
			bargs = append(bargs, id)
		}
		brows, err := tx.QueryContext(ctx, `SELECT id, data FROM session_message WHERE id IN (`+ph+`)`, bargs...)
		if err != nil {
			return updated, removedBytes, err
		}
		var updates []update
		for brows.Next() {
			var id, data string
			if err := brows.Scan(&id, &data); err != nil {
				brows.Close()
				return updated, removedBytes, err
			}
			if next, changed := stripReasoning(data); changed {
				updates = append(updates, update{id: id, data: next})
				removedBytes += int64(len(data) - len(next))
			}
		}
		if err := brows.Err(); err != nil {
			brows.Close()
			return updated, removedBytes, err
		}
		brows.Close()

		for _, u := range updates {
			if _, err := tx.ExecContext(ctx, `UPDATE session_message SET data = ? WHERE id = ?`, u.data, u.id); err != nil {
				return updated, removedBytes, err
			}
			updated++
		}
	}

	// session_part rows are not used by the current schema version much, but
	// clean up the equivalent reasoning parts when they exist.
	if d.HasTable("session_part") {
		q := `DELETE FROM session_part WHERE type='reasoning' AND time_created < ?
		      AND session_id IN (SELECT id FROM session_v2 WHERE time_updated < ?)`
		if _, err := tx.ExecContext(ctx, q, cutoff, cutoff); err != nil {
			return updated, removedBytes, err
		}
	}
	return updated, removedBytes, nil
}

func stripReasoning(data string) (string, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		return data, false
	}
	raw, ok := doc["content"]
	if !ok {
		return data, false
	}
	var content []json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil || len(content) == 0 {
		return data, false
	}
	kept := make([]json.RawMessage, 0, len(content))
	for _, item := range content {
		var t struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &t); err == nil && t.Type == "reasoning" {
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == len(content) {
		return data, false
	}
	newContent, err := marshalNoEscape(kept)
	if err != nil {
		return data, false
	}
	doc["content"] = newContent
	newData, err := marshalNoEscape(doc)
	if err != nil {
		return data, false
	}
	return string(newData), true
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
