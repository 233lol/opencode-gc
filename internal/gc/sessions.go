package gc

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"opencode-gc/internal/dbgc"
	"opencode-gc/internal/paths"
	"opencode-gc/internal/report"
	"opencode-gc/internal/service"
)

// SessionListOptions controls `sessions list`.
type SessionListOptions struct {
	Version   string
	Paths     paths.Paths
	Now       time.Time
	Sort      string
	Limit     int
	Project   string
	OlderThan time.Duration
	Verbose   bool
}

// SessionDeleteOptions controls `sessions delete`.
type SessionDeleteOptions struct {
	Version      string
	Paths        paths.Paths
	Now          time.Time
	IDs          []string
	Apply        bool
	Force        bool
	WithChildren bool
	Vacuum       bool
	Backup       bool
	BackupKeep   int
	AllowRunning bool
	Verbose      bool
}

type sessionData struct {
	meta    *dbgc.Meta
	stats   map[string]dbgc.SessionStat
	rows    []report.SessionRow
	dbStats dbgc.Stats
}

// ListSessions prints an analysis of sessions sorted by size or another metric.
func ListSessions(ctx context.Context, opts SessionListOptions) report.SessionList {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	sortKey := normalizeSort(opts.Sort)
	rep := report.SessionList{
		Tool:     "opencode-gc",
		Version:  opts.Version,
		Command:  "sessions list",
		DataDir:  opts.Paths.Data,
		DBPath:   opts.Paths.DB,
		Sort:     sortKey,
		Limit:    opts.Limit,
		Rows:     []report.SessionRow{},
		Warnings: []string{},
		Errors:   []string{},
		Verbose:  opts.Verbose,
	}

	db, data, err := loadSessionData(ctx, opts.Paths.DB, now)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		rep.ExitCode = 1
		return rep
	}
	db.Close()

	rows := filterSessionRows(data.rows, opts.Project, opts.OlderThan, now)
	if err := sortSessionRows(rows, sortKey); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		rep.ExitCode = 1
		return rep
	}

	rep.TotalSessions = len(rows)
	rep.TotalBytes = sumRowBytes(rows)
	if opts.Limit < 0 {
		opts.Limit = 0
	}
	if opts.Limit > 0 && len(rows) > opts.Limit {
		rows = rows[:opts.Limit]
	}
	rep.Limit = opts.Limit
	rep.Rows = rows
	rep.Shown = len(rows)
	rep.ShownBytes = sumRowBytes(rows)
	return rep
}

// DeleteSessions deletes explicitly named sessions, with a preview by default.
func DeleteSessions(ctx context.Context, opts SessionDeleteOptions) report.SessionDeleteReport {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	rep := report.SessionDeleteReport{
		SessionList: report.SessionList{
			Tool:     "opencode-gc",
			Version:  opts.Version,
			Command:  "sessions delete",
			DataDir:  opts.Paths.Data,
			DBPath:   opts.Paths.DB,
			Sort:     "size",
			Rows:     []report.SessionRow{},
			Warnings: []string{},
			Errors:   []string{},
			Verbose:  opts.Verbose,
		},
		Applied:      opts.Apply,
		Force:        opts.Force,
		WithChildren: opts.WithChildren,
		Skipped:      []report.SkippedSession{},
	}
	if opts.BackupKeep <= 0 {
		opts.BackupKeep = 3
	}
	if len(opts.IDs) == 0 {
		rep.Errors = append(rep.Errors, "没有指定 session ID")
		rep.ExitCode = 1
		return rep
	}

	db, data, err := loadSessionData(ctx, opts.Paths.DB, now)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		rep.ExitCode = 1
		return rep
	}

	rowByID := make(map[string]report.SessionRow, len(data.rows))
	for _, r := range data.rows {
		rowByID[r.ID] = r
	}

	ids := uniqueIDs(opts.IDs)
	target := make(map[string]bool)
	skipped := make(map[string]string)
	var targetIDs []string
	detachedChildren := 0

	for _, id := range ids {
		s, ok := data.meta.Sessions[id]
		if !ok {
			skipped[id] = "不存在"
			continue
		}
		row := rowByID[id]
		if data.meta.Protected[id] {
			skipped[id] = "当前会话或受保护链，不能删除"
			continue
		}
		if !opts.Force && row.Recent {
			skipped[id] = "24h 内更新过（使用 --force 可覆盖）"
			continue
		}

		if opts.WithChildren {
			desc := descendants(data.meta, id)
			bad := ""
			for _, child := range desc {
				if data.meta.Protected[child] {
					bad = "包含受保护的子会话 " + child
					break
				}
				if !opts.Force && rowByID[child].Recent {
					bad = "包含 24h 内更新的子会话 " + child
					break
				}
			}
			if bad != "" {
				skipped[id] = bad
				continue
			}
			for _, child := range desc {
				if !target[child] {
					target[child] = true
					targetIDs = append(targetIDs, child)
				}
			}
		} else if row.Children > 0 {
			detachedChildren++
		}

		if !target[id] {
			target[id] = true
			targetIDs = append(targetIDs, id)
		}
		_ = s
	}

	if detachedChildren > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%d 个待删除会话仍有子会话；未指定 --with-children 时，子会话会保留并转为顶级会话。", detachedChildren))
	}

	rows := make([]report.SessionRow, 0, len(targetIDs))
	for _, id := range targetIDs {
		if row, ok := rowByID[id]; ok {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TotalBytes > rows[j].TotalBytes })
	rep.Rows = rows
	rep.TotalSessions = len(rows)
	rep.TotalBytes = sumRowBytes(rows)
	rep.Shown = len(rows)
	rep.ShownBytes = rep.TotalBytes

	skippedIDs := make([]string, 0, len(skipped))
	for id := range skipped {
		skippedIDs = append(skippedIDs, id)
	}
	sort.Strings(skippedIDs)
	for _, id := range skippedIDs {
		rep.Skipped = append(rep.Skipped, report.SkippedSession{ID: id, Reason: skipped[id]})
	}

	if len(targetIDs) == 0 {
		if opts.Apply {
			rep.ExitCode = 2
		}
		db.Close()
		return rep
	}

	// Preview: report the service state but do not write.
	info := service.Detect(ctx, opts.Paths.State)
	if !opts.Apply {
		if info.Running {
			rep.Warnings = append(rep.Warnings, "OpenCode 服务正在运行；执行 --apply 时数据库写入会被跳过（可用 --allow-running 覆盖）。")
		}
		db.Close()
		return rep
	}
	rep.Applied = true
	db.Close()

	if info.Running && !opts.AllowRunning {
		rep.Warnings = append(rep.Warnings, "OpenCode 服务正在运行，已跳过会话删除。请先退出 OpenCode 或执行 `opencode service stop`。")
		rep.ExitCode = 2
		return rep
	}

	rw, err := dbgc.Open(ctx, opts.Paths.DB, false)
	if err != nil {
		rep.Errors = append(rep.Errors, "打开数据库失败: "+err.Error())
		rep.ExitCode = 1
		return rep
	}
	defer rw.Close()

	candidates := make([]dbgc.SessionCandidate, 0, len(targetIDs))
	for _, id := range targetIDs {
		s, ok := data.meta.Sessions[id]
		if !ok {
			continue
		}
		st := data.stats[id]
		candidates = append(candidates, dbgc.SessionCandidate{
			Session:  s,
			Messages: st.Messages,
			Bytes:    st.MessageBytes,
		})
	}

	cr, err := rw.Clean(ctx, dbgc.CleanOptions{
		Now:           now,
		DeleteOrphans: true,
		OrphanRules:   dbgc.OrphanRules(rw),
		Sessions:      candidates,
		Protected:     data.meta.Protected,
		Force:         opts.Force,
		Backup:        opts.Backup,
		BackupDir:     opts.Paths.BackupDir,
		BackupKeep:    opts.BackupKeep,
		Vacuum:        opts.Vacuum,
		Running:       info.Running,
		AllowRunning:  opts.AllowRunning,
	})
	rep.BackupPath = cr.BackupPath
	rep.BackupBytes = cr.BackupBytes
	rep.VacuumDone = cr.VacuumDone
	rep.DBRows = cr.Rows
	rep.Warnings = append(rep.Warnings, cr.Warnings...)
	rep.Errors = append(rep.Errors, cr.Errors...)
	if err != nil {
		rep.Errors = append(rep.Errors, "会话删除失败: "+err.Error())
		rep.ExitCode = 1
		return rep
	}
	if cr.Skipped {
		rep.ExitCode = 2
	}

	remaining, err := rw.CountSessions(ctx, targetIDs)
	if err == nil {
		rep.DeletedSessions = int64(len(targetIDs)) - remaining
	} else {
		rep.DeletedSessions = int64(len(targetIDs))
	}

	after := rw.GetStats(ctx)
	rep.DBSizeBefore = data.dbStats.DBSize
	rep.WALSizeBefore = data.dbStats.WALSize
	rep.DBSizeAfter = after.DBSize
	rep.WALSizeAfter = after.WALSize
	if after.FreeBytes > 0 && !rep.VacuumDone {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"数据库仍有 %s 空闲页未回收；停止 OpenCode 服务后执行 `opencode-gc vacuum --apply`。",
			report.HumanBytes(after.FreeBytes)))
	}
	if rep.ExitCode == 0 && len(rep.Errors) > 0 {
		rep.ExitCode = 2
	}
	return rep
}

func loadSessionData(ctx context.Context, dbPath string, now time.Time) (*dbgc.DB, *sessionData, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil, fmt.Errorf("数据库不存在: %s", dbPath)
	}
	db, err := dbgc.Open(ctx, dbPath, true)
	if err != nil {
		return nil, nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	if err := db.CheckV2(ctx); err != nil {
		db.Close()
		return nil, nil, err
	}
	meta, err := dbgc.LoadMeta(ctx, db)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	stats, err := dbgc.LoadSessionStats(ctx, db)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	rows := buildSessionRows(meta, stats, now)
	return db, &sessionData{
		meta:    meta,
		stats:   stats,
		rows:    rows,
		dbStats: db.GetStats(ctx),
	}, nil
}

func buildSessionRows(meta *dbgc.Meta, stats map[string]dbgc.SessionStat, now time.Time) []report.SessionRow {
	recentCutoff := now.Add(-24 * time.Hour).UnixMilli()
	rows := make([]report.SessionRow, 0, len(meta.Sessions))
	for _, s := range meta.Sessions {
		st := stats[s.ID]
		p := meta.Projects[s.ProjectID]
		project := p.Name
		if project == "" {
			project = p.Worktree
		}
		rows = append(rows, report.SessionRow{
			ID:               s.ID,
			Slug:             s.Slug,
			Title:            s.Title,
			ProjectID:        s.ProjectID,
			Project:          project,
			Worktree:         p.Worktree,
			Directory:        s.Directory,
			ParentID:         s.ParentID,
			Created:          s.Created,
			Updated:          s.Updated,
			Archived:         s.Archived,
			Messages:         st.Messages,
			MessageBytes:     st.MessageBytes,
			BlobBytes:        st.BlobBytes,
			PartBytes:        st.PartBytes,
			InstructionBytes: st.InstructionBytes,
			TotalBytes:       st.TotalBytes,
			Tokens:           s.Tokens,
			Cost:             s.Cost,
			Children:         st.Children,
			Protected:        meta.Protected[s.ID],
			Recent:           s.Updated >= recentCutoff,
		})
	}
	return rows
}

func filterSessionRows(rows []report.SessionRow, project string, olderThan time.Duration, now time.Time) []report.SessionRow {
	q := strings.ToLower(strings.TrimSpace(project))
	cutoff := int64(0)
	if olderThan > 0 {
		cutoff = now.Add(-olderThan).UnixMilli()
	}
	out := make([]report.SessionRow, 0, len(rows))
	for _, r := range rows {
		if q != "" {
			hay := strings.Join([]string{r.Project, r.Worktree, r.Directory, r.ID, r.Slug, r.Title}, "\n")
			if !strings.Contains(strings.ToLower(hay), q) {
				continue
			}
		}
		if cutoff > 0 && r.Updated >= cutoff {
			continue
		}
		out = append(out, r)
	}
	return out
}

func sortSessionRows(rows []report.SessionRow, key string) error {
	switch key {
	case "size":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].TotalBytes != rows[j].TotalBytes {
				return rows[i].TotalBytes > rows[j].TotalBytes
			}
			return rows[i].Updated > rows[j].Updated
		})
	case "messages":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Messages > rows[j].Messages })
	case "updated":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Updated > rows[j].Updated })
	case "created":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Created > rows[j].Created })
	case "cost":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Cost > rows[j].Cost })
	case "tokens":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Tokens > rows[j].Tokens })
	default:
		return fmt.Errorf("无效的排序字段 %q（可用 size、messages、updated、created、cost、tokens）", key)
	}
	return nil
}

func normalizeSort(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return "size"
	}
	return key
}

func sumRowBytes(rows []report.SessionRow) int64 {
	var n int64
	for _, r := range rows {
		n += r.TotalBytes
	}
	return n
}

func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func descendants(meta *dbgc.Meta, root string) []string {
	var out []string
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, child := range meta.Children[id] {
			if seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}
