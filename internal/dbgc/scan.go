package dbgc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"opencode-gc/internal/fsgc"
)

// Project is a row from the project table.
type Project struct {
	ID       string
	Worktree string
	Created  int64
}

// Session is a row from session_v2.
type Session struct {
	ID        string
	ProjectID string
	ParentID  string
	ForkID    string
	Created   int64
	Updated   int64
	Archived  int64
}

// Snapshot is an in-memory view of the rows needed for planning.
type Snapshot struct {
	Projects          map[string]Project
	Sessions          map[string]Session
	Children          map[string][]string
	Protected         map[string]bool
	ProtectedProjects map[string]bool
	Refs              *fsgc.RefIndex
}

// LoadSnapshot loads projects, sessions, protection info and file references.
func LoadSnapshot(ctx context.Context, d *DB) (*Snapshot, error) {
	if err := d.CheckV2(ctx); err != nil {
		return nil, err
	}
	s := &Snapshot{
		Projects:          make(map[string]Project),
		Sessions:          make(map[string]Session),
		Children:          make(map[string][]string),
		Protected:         make(map[string]bool),
		ProtectedProjects: make(map[string]bool),
		Refs:              fsgc.NewRefIndex(),
	}

	rows, err := d.sql.QueryContext(ctx, `SELECT id, worktree, time_created FROM project`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Worktree, &p.Created); err != nil {
			rows.Close()
			return nil, err
		}
		s.Projects[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = d.sql.QueryContext(ctx, `
		SELECT id, project_id, coalesce(parent_id,''), coalesce(fork_session_id,''),
		       time_created, time_updated, coalesce(time_archived,0)
		FROM session_v2`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.ProjectID, &x.ParentID, &x.ForkID, &x.Created, &x.Updated, &x.Archived); err != nil {
			rows.Close()
			return nil, err
		}
		s.Sessions[x.ID] = x
		if x.ParentID != "" {
			s.Children[x.ParentID] = append(s.Children[x.ParentID], x.ID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Protect the session that is running this tool, its ancestors and its
	// descendants (subagent sessions).
	if current := strings.TrimSpace(os.Getenv("OPENCODE_SESSION_ID")); current != "" {
		for id := current; id != ""; {
			s.Protected[id] = true
			next := s.Sessions[id]
			if next.ID == "" || next.ParentID == id {
				break
			}
			id = next.ParentID
		}
		queue := []string{current}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			for _, child := range s.Children[id] {
				if !s.Protected[child] {
					s.Protected[child] = true
					queue = append(queue, child)
				}
			}
		}
	}

	// Protect the project that contains the current working directory.
	if wd, err := os.Getwd(); err == nil {
		for _, p := range s.Projects {
			if samePath(p.Worktree, wd) {
				s.ProtectedProjects[p.ID] = true
			}
		}
	}

	if err := s.loadFileRefs(ctx, d); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Snapshot) loadFileRefs(ctx context.Context, d *DB) error {
	queries := []string{}
	if d.HasTable("session_message") {
		queries = append(queries, `SELECT data FROM session_message WHERE data LIKE '%sh_%' OR data LIKE '%tool-output%'`)
	}
	if d.HasTable("session_part") {
		queries = append(queries, `SELECT data FROM session_part WHERE data LIKE '%sh_%' OR data LIKE '%tool-output%'`)
	}
	for _, q := range queries {
		rows, err := d.sql.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				rows.Close()
				return err
			}
			extractRefs(data, s.Refs)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

var (
	reShell    = regexp.MustCompile(`sh_[A-Za-z0-9]+\.out`)
	reTool     = regexp.MustCompile(`tool-output[\\/]+([A-Za-z0-9_.@%\-]+(?:[\\/]+[A-Za-z0-9_.@%\-]+)*)`)
	reToolFile = regexp.MustCompile(`tool_[0-9a-f]{12}[A-Za-z0-9_.\-]*`)
)

func extractRefs(data string, refs *fsgc.RefIndex) {
	if refs == nil {
		return
	}
	// JSON escapes backslashes; normalise double backslashes so both Windows
	// and POSIX paths match.
	norm := strings.ReplaceAll(data, `\\`, `\`)
	for _, m := range reShell.FindAllString(norm, -1) {
		refs.Add(m)
	}
	for _, m := range reTool.FindAllStringSubmatch(norm, -1) {
		if len(m) > 1 {
			refs.Add(m[1])
		}
	}
	// tool-output files are flat and named tool_<12 hex chars>...
	for _, m := range reToolFile.FindAllString(norm, -1) {
		refs.Add(m)
	}
}

// LiveProjects returns a set of project IDs.
func (s *Snapshot) LiveProjects() map[string]struct{} {
	out := make(map[string]struct{}, len(s.Projects))
	for id := range s.Projects {
		out[id] = struct{}{}
	}
	return out
}

// LiveSessions returns a set of session IDs.
func (s *Snapshot) LiveSessions() map[string]struct{} {
	out := make(map[string]struct{}, len(s.Sessions))
	for id := range s.Sessions {
		out[id] = struct{}{}
	}
	return out
}

// OrphanRule is one referential-integrity cleanup for the database.
type OrphanRule struct {
	Name        string
	Description string
	CountSQL    string
	BytesSQL    string
	DeleteSQL   string
	Count       int64
	Bytes       int64
}

// ProjectCandidate is a project with no sessions.
type ProjectCandidate struct {
	Project
}

// SessionCandidate is a session that can be deleted.
type SessionCandidate struct {
	Session
	Messages int64
	Bytes    int64
}

// Plan is everything the cleaner knows before it changes anything.
type Plan struct {
	Snapshot *Snapshot
	Stats    Stats

	Orphans  []OrphanRule
	Projects []ProjectCandidate
	Sessions []SessionCandidate
	Archived []SessionCandidate

	ReasoningCount      int64
	ReasoningBytes      int64
	ReasoningCutoff     int64
	ReasoningSessionIDs []string

	DBCacheKeys  []string
	DBCacheBytes int64

	Protected map[string]bool
}

// PlanOptions controls candidate selection.
type PlanOptions struct {
	Now          time.Time
	ProjectAge   time.Duration
	SessionAge   time.Duration
	ArchivedAge  time.Duration
	ReasoningAge time.Duration
}

// BuildPlan scans the database and builds a cleanup plan.
func BuildPlan(ctx context.Context, d *DB, opts PlanOptions) (*Plan, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	snap, err := LoadSnapshot(ctx, d)
	if err != nil {
		return nil, err
	}
	p := &Plan{
		Snapshot:  snap,
		Stats:     d.GetStats(ctx),
		Protected: snap.Protected,
	}

	p.Orphans = buildOrphanRules(d)
	for i := range p.Orphans {
		r := &p.Orphans[i]
		if r.CountSQL != "" {
			_ = d.sql.QueryRowContext(ctx, r.CountSQL).Scan(&r.Count)
		}
		if r.BytesSQL != "" {
			_ = d.sql.QueryRowContext(ctx, r.BytesSQL).Scan(&r.Bytes)
		}
	}

	msgStats := sessionMessageStats(ctx, d)

	// Projects without sessions.
	if opts.ProjectAge > 0 {
		cutoff := now.Add(-opts.ProjectAge).UnixMilli()
		projectsWithSessions := make(map[string]bool)
		for _, s := range snap.Sessions {
			projectsWithSessions[s.ProjectID] = true
		}
		for _, proj := range snap.Projects {
			if projectsWithSessions[proj.ID] || snap.ProtectedProjects[proj.ID] {
				continue
			}
			if proj.Created >= cutoff {
				continue
			}
			p.Projects = append(p.Projects, ProjectCandidate{Project: proj})
		}
		sort.Slice(p.Projects, func(i, j int) bool { return p.Projects[i].Created < p.Projects[j].Created })
	}

	// Sessions that have not been used for a while.
	if opts.SessionAge > 0 {
		age := maxDuration(opts.SessionAge, 24*time.Hour)
		cutoff := now.Add(-age).UnixMilli()
		for _, s := range snap.Sessions {
			if snap.Protected[s.ID] || s.Updated >= cutoff {
				continue
			}
			msgs, bytes := msgStats[s.ID][0], msgStats[s.ID][1]
			p.Sessions = append(p.Sessions, SessionCandidate{Session: s, Messages: msgs, Bytes: bytes})
		}
		sort.Slice(p.Sessions, func(i, j int) bool { return p.Sessions[i].Updated < p.Sessions[j].Updated })
	}

	// Archived sessions.
	if opts.ArchivedAge > 0 {
		age := maxDuration(opts.ArchivedAge, 24*time.Hour)
		cutoff := now.Add(-age).UnixMilli()
		for _, s := range snap.Sessions {
			if snap.Protected[s.ID] || s.Archived == 0 || s.Archived >= cutoff {
				continue
			}
			msgs, bytes := msgStats[s.ID][0], msgStats[s.ID][1]
			p.Archived = append(p.Archived, SessionCandidate{Session: s, Messages: msgs, Bytes: bytes})
		}
		sort.Slice(p.Archived, func(i, j int) bool { return p.Archived[i].Archived < p.Archived[j].Archived })
	}

	// Reasoning text in old messages.
	if opts.ReasoningAge > 0 {
		age := maxDuration(opts.ReasoningAge, time.Hour)
		cutoff := now.Add(-age).UnixMilli()
		p.ReasoningCutoff = cutoff

		protected := make([]string, 0, len(snap.Protected))
		for id := range snap.Protected {
			protected = append(protected, id)
		}
		sessionFilter := `session_id IN (SELECT id FROM session_v2 WHERE time_updated < ?)`
		args := []any{cutoff}
		if len(protected) > 0 {
			sessionFilter += ` AND session_id NOT IN (` + placeholders(len(protected)) + `)`
			for _, id := range protected {
				args = append(args, id)
			}
		}
		q := `SELECT data FROM session_message
		      WHERE type='assistant' AND time_created < ?
		        AND data LIKE '%"type":"reasoning"%'
		        AND ` + sessionFilter
		argv := append([]any{cutoff}, args...)
		rows, err := d.sql.QueryContext(ctx, q, argv...)
		if err == nil {
			for rows.Next() {
				var data string
				if rows.Scan(&data) != nil {
					continue
				}
				if next, changed := stripReasoning(data); changed {
					p.ReasoningCount++
					p.ReasoningBytes += int64(len(data) - len(next))
				}
			}
			rows.Close()
		}

		rrows, err := d.sql.QueryContext(ctx,
			`SELECT id FROM session_v2 WHERE time_updated < ?`, cutoff)
		if err == nil {
			for rrows.Next() {
				var id string
				if rrows.Scan(&id) == nil && !snap.Protected[id] {
					p.ReasoningSessionIDs = append(p.ReasoningSessionIDs, id)
				}
			}
			rrows.Close()
		}
	}

	// Regenerable model catalogue cache.
	if d.HasTable("kv") {
		rows, err := d.sql.QueryContext(ctx, `SELECT key, length(value) FROM kv WHERE key LIKE 'models-dev:%' ORDER BY key`)
		if err == nil {
			for rows.Next() {
				var key string
				var n int64
				if rows.Scan(&key, &n) == nil {
					p.DBCacheKeys = append(p.DBCacheKeys, key)
					p.DBCacheBytes += n
				}
			}
			rows.Close()
		}
	}

	return p, nil
}

func sessionMessageStats(ctx context.Context, d *DB) map[string][2]int64 {
	out := make(map[string][2]int64)
	if !d.HasTable("session_message") {
		return out
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT session_id, count(*), coalesce(sum(length(data)),0)
		FROM session_message GROUP BY session_id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count, bytes int64
		if rows.Scan(&id, &count, &bytes) == nil {
			out[id] = [2]int64{count, bytes}
		}
	}
	return out
}

func buildOrphanRules(d *DB) []OrphanRule {
	var rules []OrphanRule
	add := func(name, desc, countSQL, bytesSQL, deleteSQL string) {
		rules = append(rules, OrphanRule{
			Name: name, Description: desc,
			CountSQL: countSQL, BytesSQL: bytesSQL, DeleteSQL: deleteSQL,
		})
	}

	if d.HasTable("session_message") && d.HasTable("session_v2") {
		add("orphan messages", "父 session 不存在的消息",
			`SELECT count(*) FROM session_message WHERE session_id NOT IN (SELECT id FROM session_v2)`,
			`SELECT coalesce(sum(length(data)),0) FROM session_message WHERE session_id NOT IN (SELECT id FROM session_v2)`,
			`DELETE FROM session_message WHERE session_id NOT IN (SELECT id FROM session_v2)`)
	}
	if d.HasTable("session_part") && d.HasTable("session_message") && d.HasTable("session_v2") {
		where := `session_id NOT IN (SELECT id FROM session_v2) OR message_id NOT IN (SELECT id FROM session_message)`
		add("orphan parts", "父 message 或 session 不存在的 part",
			`SELECT count(*) FROM session_part WHERE `+where,
			`SELECT coalesce(sum(length(data)),0) FROM session_part WHERE `+where,
			`DELETE FROM session_part WHERE `+where)
	}
	if d.HasTable("blob_ref") && d.HasTable("session_v2") && d.HasTable("session_message") && d.HasTable("blob") {
		where := `session_id NOT IN (SELECT id FROM session_v2)
			OR message_id NOT IN (SELECT id FROM session_message)
			OR hash NOT IN (SELECT hash FROM blob)`
		add("orphan blob refs", "父记录不存在的 blob 引用",
			`SELECT count(*) FROM blob_ref WHERE `+where, "",
			`DELETE FROM blob_ref WHERE `+where)
	}
	if d.HasTable("blob") && d.HasTable("blob_ref") {
		where := `hash NOT IN (SELECT hash FROM blob_ref)`
		add("unreferenced blobs", "没有任何 blob_ref 引用的 blob",
			`SELECT count(*) FROM blob WHERE `+where,
			`SELECT coalesce(sum(bytes),0) FROM blob WHERE `+where,
			`DELETE FROM blob WHERE `+where)
	}
	if d.HasTable("instruction_entry") && d.HasTable("session_v2") {
		where := `session_id NOT IN (SELECT id FROM session_v2)`
		add("orphan instruction entries", "父 session 不存在的 instruction entry",
			`SELECT count(*) FROM instruction_entry WHERE `+where, "",
			`DELETE FROM instruction_entry WHERE `+where)
	}
	if d.HasTable("instruction_state") && d.HasTable("session_v2") {
		where := `session_id NOT IN (SELECT id FROM session_v2)`
		add("orphan instruction states", "父 session 不存在的 instruction state",
			`SELECT count(*) FROM instruction_state WHERE `+where, "",
			`DELETE FROM instruction_state WHERE `+where)
	}
	if d.HasTable("instruction_blob") && d.HasTable("instruction_state") {
		where := `NOT EXISTS (
			SELECT 1 FROM instruction_state s
			WHERE s.initial_values LIKE '%"' || instruction_blob.hash || '"%'
			   OR s.current_values LIKE '%"' || instruction_blob.hash || '"%'
		)`
		if d.HasTable("instruction_entry") {
			where += ` AND NOT EXISTS (SELECT 1 FROM instruction_entry e WHERE e.value = instruction_blob.hash)`
		}
		add("unused instruction blobs", "没有被任何 instruction 状态引用的 blob",
			`SELECT count(*) FROM instruction_blob WHERE `+where,
			`SELECT coalesce(sum(length(value)),0) FROM instruction_blob WHERE `+where,
			`DELETE FROM instruction_blob WHERE `+where)
	}
	if d.HasTable("event") && d.HasTable("session_v2") && d.HasTable("project") {
		where := `aggregate_id NOT IN (SELECT id FROM session_v2 UNION SELECT id FROM project)`
		add("orphan events", "aggregate 不存在的 event",
			`SELECT count(*) FROM event WHERE `+where,
			`SELECT coalesce(sum(length(data)),0) FROM event WHERE `+where,
			`DELETE FROM event WHERE `+where)
	}
	if d.HasTable("event_sequence") && d.HasTable("session_v2") && d.HasTable("project") {
		where := `aggregate_id NOT IN (SELECT id FROM session_v2 UNION SELECT id FROM project)`
		add("orphan event sequences", "aggregate 不存在的 event sequence",
			`SELECT count(*) FROM event_sequence WHERE `+where, "",
			`DELETE FROM event_sequence WHERE `+where)
	}
	if d.HasTable("job") && d.HasTable("session_v2") {
		where := `session_id NOT IN (SELECT id FROM session_v2)
			OR (child_session_id IS NOT NULL AND child_session_id NOT IN (SELECT id FROM session_v2))`
		add("orphan jobs", "父 session 不存在的后台任务",
			`SELECT count(*) FROM job WHERE `+where, "",
			`DELETE FROM job WHERE `+where)
	}
	if d.HasTable("schedule") && d.HasTable("session_v2") {
		where := `session_id IS NOT NULL AND session_id NOT IN (SELECT id FROM session_v2)`
		add("orphan schedules", "父 session 不存在的计划任务",
			`SELECT count(*) FROM schedule WHERE `+where, "",
			`DELETE FROM schedule WHERE `+where)
	}
	if d.HasTable("session_inbox") && d.HasTable("session_v2") {
		where := `session_id NOT IN (SELECT id FROM session_v2)`
		add("orphan inbox rows", "父 session 不存在的 inbox 记录",
			`SELECT count(*) FROM session_inbox WHERE `+where,
			`SELECT coalesce(sum(length(payload)),0) FROM session_inbox WHERE `+where,
			`DELETE FROM session_inbox WHERE `+where)
	}
	for _, pair := range []struct {
		table, name, desc string
	}{
		{"permission", "orphan permissions", "项目不存在的权限记录"},
		{"worktree", "orphan worktrees", "项目不存在的工作树记录"},
		{"project_directory", "orphan project directories", "项目不存在的目录记录"},
	} {
		if d.HasTable(pair.table) && d.HasTable("project") {
			where := `project_id NOT IN (SELECT id FROM project)`
			add(pair.name, pair.desc,
				`SELECT count(*) FROM `+pair.table+` WHERE `+where, "",
				`DELETE FROM `+pair.table+` WHERE `+where)
		}
	}
	return rules
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, err := filepath.Abs(a)
	if err != nil {
		aa = a
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		bb = b
	}
	return strings.EqualFold(filepath.ToSlash(filepath.Clean(aa)), filepath.ToSlash(filepath.Clean(bb)))
}

// FormatSessionSample renders a short, readable session sample.
func FormatSessionSample(s SessionCandidate) string {
	return fmt.Sprintf("%s  %s", s.ID, time.UnixMilli(s.Updated).Format("2006-01-02 15:04"))
}
