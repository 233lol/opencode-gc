// Package gc orchestrates filesystem and database cleanup.
package gc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"opencode-gc/internal/dbgc"
	"opencode-gc/internal/fsgc"
	"opencode-gc/internal/paths"
	"opencode-gc/internal/report"
	"opencode-gc/internal/service"
)

// Selection says which categories the user asked to clean.
type Selection struct {
	Tmp        bool
	Logs       bool
	Shell      bool
	ToolOutput bool
	Repos      bool

	DBOrphans  bool
	DBProjects bool
	DBCache    bool

	Sessions  bool
	Archived  bool
	Reasoning bool

	Vacuum bool
}

// DefaultSelection is the safe set used by `clean` without category flags.
func DefaultSelection() Selection {
	return Selection{Tmp: true, Logs: true, Shell: true, DBOrphans: true}
}

// AllSelection covers all non-history garbage. History deletion stays behind
// --older-than or the explicit --sessions/--archived/--strip-reasoning flags.
func AllSelection() Selection {
	return Selection{
		Tmp: true, Logs: true, Shell: true, ToolOutput: true, Repos: true,
		DBOrphans: true, DBProjects: true, DBCache: true,
		Vacuum: true,
	}
}

// UnifiedSelection is used by --older-than when no explicit category flags are
// given: everything time-based, plus orphan cleanup and vacuum. Repos and the
// regenerable database cache stay opt-in.
func UnifiedSelection() Selection {
	return Selection{
		Tmp: true, Logs: true, Shell: true, ToolOutput: true,
		DBOrphans: true, DBProjects: true,
		Sessions: true, Archived: true, Reasoning: true,
		Vacuum: true,
	}
}

// ForCategory maps a report category to the selection.
func (s Selection) ForCategory(category string) bool {
	switch category {
	case "tmp":
		return s.Tmp
	case "logs":
		return s.Logs
	case "shell":
		return s.Shell
	case "tool-output":
		return s.ToolOutput
	case "repos":
		return s.Repos
	case "db-orphans":
		return s.DBOrphans
	case "db-projects":
		return s.DBProjects
	case "db-cache":
		return s.DBCache
	case "db-sessions":
		return s.Sessions
	case "db-archived":
		return s.Archived
	case "db-reasoning":
		return s.Reasoning
	case "db-vacuum":
		return s.Vacuum
	}
	return false
}

// AnyDB reports whether a database category is selected.
func (s Selection) AnyDB() bool {
	return s.DBOrphans || s.DBProjects || s.DBCache || s.Sessions || s.Archived || s.Reasoning || s.Vacuum
}

// Any reports whether anything is selected.
func (s Selection) Any() bool {
	return s.Tmp || s.Logs || s.Shell || s.ToolOutput || s.Repos || s.AnyDB()
}

// Options is the complete configuration for a run.
type Options struct {
	Command string
	Version string
	Paths   paths.Paths
	Now     time.Time

	FileAge      time.Duration
	ProjectAge   time.Duration
	SessionAge   time.Duration
	ArchivedAge  time.Duration
	ReasoningAge time.Duration

	Selection Selection
	Apply     bool

	Backup     bool
	BackupKeep int
	BackupOut  string

	AllowRunning bool
	Verbose      bool
}

// Run performs the requested command and returns a report. Fatal problems are
// placed in the report rather than returned separately.
func Run(ctx context.Context, opts Options) report.Report {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.BackupKeep <= 0 {
		opts.BackupKeep = 3
	}
	switch opts.Command {
	case "backup":
		return runBackup(ctx, opts)
	case "vacuum":
		return runVacuum(ctx, opts)
	default:
		return runScanClean(ctx, opts)
	}
}

func baseReport(opts Options) report.Report {
	return report.Report{
		Tool:     "opencode-gc",
		Version:  opts.Version,
		Command:  opts.Command,
		DataDir:  opts.Paths.Data,
		TmpDir:   opts.Paths.Tmp,
		DBPath:   opts.Paths.DB,
		Findings: []report.Finding{},
		Warnings: []string{},
		Errors:   []string{},
		Verbose:  opts.Verbose,
	}
}

func runScanClean(ctx context.Context, opts Options) report.Report {
	rep := baseReport(opts)
	now := opts.Now

	var ro *dbgc.DB
	var plan *dbgc.Plan
	var stats dbgc.Stats

	if _, err := os.Stat(opts.Paths.DB); err == nil {
		rep.DBExists = true
		db, err := dbgc.Open(ctx, opts.Paths.DB, true)
		if err != nil {
			rep.Warnings = append(rep.Warnings, "无法打开数据库: "+err.Error())
		} else {
			ro = db
			stats = db.GetStats(ctx)
			if err := db.CheckV2(ctx); err != nil {
				rep.Warnings = append(rep.Warnings, "数据库不是受支持的 OpenCode V2 结构，已跳过数据库分析: "+err.Error())
			} else {
				p, err := dbgc.BuildPlan(ctx, db, dbgc.PlanOptions{
					Now:          now,
					ProjectAge:   opts.ProjectAge,
					SessionAge:   opts.SessionAge,
					ArchivedAge:  opts.ArchivedAge,
					ReasoningAge: opts.ReasoningAge,
				})
				if err != nil {
					rep.Warnings = append(rep.Warnings, "数据库分析失败: "+err.Error())
				} else {
					plan = p
					stats = p.Stats
				}
			}
		}
	} else if !os.IsNotExist(err) {
		rep.Warnings = append(rep.Warnings, "无法读取数据库: "+err.Error())
	} else {
		rep.Warnings = append(rep.Warnings, "数据库不存在: "+opts.Paths.DB)
	}

	rep.DBExists = rep.DBExists && ro != nil
	rep.DBSize = stats.DBSize
	rep.WALSize = stats.WALSize
	rep.FreeSize = stats.FreeBytes

	if opts.SessionAge > 0 && opts.SessionAge < 24*time.Hour {
		rep.Warnings = append(rep.Warnings, "会话最小可删年龄为 24h，已按 24h 计算。")
	}
	if opts.ArchivedAge > 0 && opts.ArchivedAge < 24*time.Hour {
		rep.Warnings = append(rep.Warnings, "归档会话最小可删年龄为 24h，已按 24h 计算。")
	}
	if opts.ReasoningAge > 0 && opts.ReasoningAge < time.Hour {
		rep.Warnings = append(rep.Warnings, "reasoning 最小压缩年龄为 1h，已按 1h 计算。")
	}

	fsFindings, fsTargets := scanFS(opts, plan)
	for _, f := range fsFindings {
		rep.Findings = append(rep.Findings, f)
	}
	if plan != nil {
		rep.Findings = append(rep.Findings, dbFindings(opts, plan)...)
	} else {
		rep.Findings = append(rep.Findings, dbUnavailableFindings(opts)...)
	}

	if !opts.Apply && opts.Selection.AnyDB() && plan != nil {
		if info := service.Detect(ctx, opts.Paths.State); info.Running {
			rep.Warnings = append(rep.Warnings,
				"OpenCode 服务正在运行；数据库清理需要先退出 OpenCode 或执行 `opencode service stop`（可用 --allow-running 强制执行结构性清理）。")
		}
	}

	if opts.Command == "scan" || !opts.Apply {
		if ro != nil {
			ro.Close()
		}
		return rep
	}

	// From here on this is a real clean.
	rep.Applied = true
	rep.DBSizeBefore = stats.DBSize
	rep.WALSizeBefore = stats.WALSize

	targets := filterTargets(fsTargets, opts.Selection)
	if plan != nil {
		targets = append(targets, extraTargets(opts, plan)...)
	}
	cutoff := now.Add(-opts.FileAge)
	dr := fsgc.Delete(targets, cutoff)
	rep.DeletedFiles = dr.Deleted
	rep.FreedFileBytes = dr.Bytes
	rep.Errors = append(rep.Errors, dr.Errors...)

	if opts.Selection.AnyDB() {
		if plan == nil {
			rep.Warnings = append(rep.Warnings, "数据库不可用，已跳过数据库清理。")
			rep.ExitCode = 2
		} else if !planHasChanges(plan, opts.Selection) && !opts.Selection.Vacuum {
			rep.DBSizeAfter = stats.DBSize
			rep.WALSizeAfter = stats.WALSize
		} else {
			if ro != nil {
				ro.Close()
				ro = nil
			}
			rw, err := dbgc.Open(ctx, opts.Paths.DB, false)
			if err != nil {
				rep.Errors = append(rep.Errors, "打开数据库失败: "+err.Error())
				rep.ExitCode = 1
			} else {
				info := service.Detect(ctx, opts.Paths.State)
				co := dbgc.CleanOptions{
					Now:              now,
					DeleteOrphans:    opts.Selection.DBOrphans,
					OrphanRules:      plan.Orphans,
					Projects:         pickProjects(plan, opts.Selection),
					Sessions:         pickSessions(plan.Sessions, opts.Selection.Sessions),
					Archived:         pickSessions(plan.Archived, opts.Selection.Archived),
					CompactReasoning: opts.Selection.Reasoning,
					ReasoningCutoff:  plan.ReasoningCutoff,
					Protected:        plan.Protected,
					DBCacheKeys:      pickCache(plan, opts.Selection),
					Vacuum:           opts.Selection.Vacuum,
					Backup:           opts.Backup,
					BackupDir:        opts.Paths.BackupDir,
					BackupKeep:       opts.BackupKeep,
					Running:          info.Running,
					AllowRunning:     opts.AllowRunning,
				}
				cr, err := rw.Clean(ctx, co)
				rep.BackupPath = cr.BackupPath
				rep.BackupBytes = cr.BackupBytes
				if err != nil {
					rep.Errors = append(rep.Errors, "数据库清理失败: "+err.Error())
					rep.ExitCode = 2
				} else {
					rep.DBChanged = cr.Changed
					rep.DBRows = cr.Rows
					rep.VacuumDone = cr.VacuumDone
					rep.Warnings = append(rep.Warnings, cr.Warnings...)
					rep.Errors = append(rep.Errors, cr.Errors...)
					if cr.Skipped {
						rep.ExitCode = 2
					}
				}
				after := rw.GetStats(ctx)
				rep.DBSizeAfter = after.DBSize
				rep.WALSizeAfter = after.WALSize
				rep.DBSize = after.DBSize
				rep.WALSize = after.WALSize
				rep.FreeSize = after.FreeBytes
				for i := range rep.Findings {
					if rep.Findings[i].Category == "db-vacuum" {
						rep.Findings[i].Bytes = after.FreeBytes + after.WALSize
						rep.Findings[i].Count = boolInt(rep.Findings[i].Bytes > 0)
					}
				}
				if after.FreeBytes > 0 && !rep.VacuumDone {
					rep.Warnings = append(rep.Warnings, fmt.Sprintf(
						"数据库仍有 %s 空闲页未回收；停止 OpenCode 服务后执行 `opencode-gc vacuum --apply`。",
						report.HumanBytes(after.FreeBytes)))
				}
				rw.Close()
			}
		}
	} else {
		rep.DBSizeAfter = stats.DBSize
		rep.WALSizeAfter = stats.WALSize
	}

	if ro != nil {
		ro.Close()
	}
	if rep.ExitCode == 0 && len(rep.Errors) > 0 {
		rep.ExitCode = 2
	}
	return rep
}

func runVacuum(ctx context.Context, opts Options) report.Report {
	rep := baseReport(opts)
	if _, err := os.Stat(opts.Paths.DB); err != nil {
		rep.Errors = append(rep.Errors, "数据库不存在: "+opts.Paths.DB)
		rep.ExitCode = 1
		return rep
	}
	rep.DBExists = true

	db, err := dbgc.Open(ctx, opts.Paths.DB, true)
	if err != nil {
		rep.Errors = append(rep.Errors, "打开数据库失败: "+err.Error())
		rep.ExitCode = 1
		return rep
	}
	stats := db.GetStats(ctx)
	rep.DBSize, rep.WALSize, rep.FreeSize = stats.DBSize, stats.WALSize, stats.FreeBytes
	db.Close()

	rep.Findings = []report.Finding{{
		Category:    "db-vacuum",
		Description: "WAL checkpoint + VACUUM，需要 OpenCode 服务已停止",
		Count:       boolInt(stats.FreeBytes+stats.WALSize > 0),
		Bytes:       stats.FreeBytes + stats.WALSize,
		Selected:    true,
		Path:        opts.Paths.DB,
	}}

	info := service.Detect(ctx, opts.Paths.State)
	if info.Running {
		rep.Warnings = append(rep.Warnings, "OpenCode 服务正在运行；VACUUM 需要先执行 `opencode service stop`。")
	}

	if !opts.Apply {
		return rep
	}
	rep.Applied = true
	rep.DBSizeBefore = stats.DBSize
	rep.WALSizeBefore = stats.WALSize

	if info.Running {
		rep.ExitCode = 2
		return rep
	}

	rw, err := dbgc.Open(ctx, opts.Paths.DB, false)
	if err != nil {
		rep.Errors = append(rep.Errors, "打开数据库失败: "+err.Error())
		rep.ExitCode = 1
		return rep
	}
	cr, err := rw.Clean(ctx, dbgc.CleanOptions{
		Now:          opts.Now,
		Vacuum:       true,
		Backup:       false,
		Protected:    map[string]bool{},
		Running:      false,
		AllowRunning: false,
	})
	after := rw.GetStats(ctx)
	rw.Close()
	if err != nil {
		rep.Errors = append(rep.Errors, "VACUUM 失败: "+err.Error())
		rep.ExitCode = 2
	}
	rep.VacuumDone = cr.VacuumDone
	rep.Warnings = append(rep.Warnings, cr.Warnings...)
	rep.DBSizeAfter = after.DBSize
	rep.WALSizeAfter = after.WALSize
	rep.DBSize = after.DBSize
	rep.WALSize = after.WALSize
	rep.FreeSize = after.FreeBytes
	return rep
}

func runBackup(ctx context.Context, opts Options) report.Report {
	rep := baseReport(opts)
	rep.Applied = true
	if _, err := os.Stat(opts.Paths.DB); err != nil {
		rep.Errors = append(rep.Errors, "数据库不存在: "+opts.Paths.DB)
		rep.ExitCode = 1
		return rep
	}
	rep.DBExists = true

	db, err := dbgc.Open(ctx, opts.Paths.DB, false)
	if err != nil {
		rep.Errors = append(rep.Errors, "打开数据库失败: "+err.Error())
		rep.ExitCode = 1
		return rep
	}
	stats := db.GetStats(ctx)
	rep.DBSize, rep.WALSize, rep.FreeSize = stats.DBSize, stats.WALSize, stats.FreeBytes

	dest := opts.BackupOut
	if dest == "" {
		dest = filepath.Join(opts.Paths.BackupDir, dbgc.BackupName(opts.Now))
	}
	size, err := db.Backup(ctx, dest)
	db.Close()
	if err != nil {
		rep.Errors = append(rep.Errors, "备份失败: "+err.Error())
		rep.ExitCode = 1
		return rep
	}
	rep.BackupPath = dest
	rep.BackupBytes = size
	rep.Warnings = append(rep.Warnings, "备份完成后可用 `sqlite3` 或 OpenCode 直接打开该文件恢复。")
	return rep
}

func scanFS(opts Options, plan *dbgc.Plan) ([]report.Finding, []fsgc.Target) {
	var refs *fsgc.RefIndex
	var liveProjects, liveSessions map[string]struct{}
	if plan != nil {
		refs = plan.Snapshot.Refs
		liveProjects = plan.Snapshot.LiveProjects()
		liveSessions = plan.Snapshot.LiveSessions()
	}
	res := fsgc.Scan(fsgc.Options{
		TmpDir:        opts.Paths.Tmp,
		LogDir:        opts.Paths.Log,
		ShellDir:      opts.Paths.Shell,
		ToolOutputDir: opts.Paths.ToolOutput,
		ReposDir:      opts.Paths.Repos,
		Age:           opts.FileAge,
		Now:           opts.Now,
		Refs:          refs,
		LiveProjects:  liveProjects,
		LiveSessions:  liveSessions,
		Verbose:       opts.Verbose,
	})
	for i := range res.Findings {
		res.Findings[i].Selected = opts.Selection.ForCategory(res.Findings[i].Category)
	}
	return res.Findings, res.Targets
}

func dbFindings(opts Options, plan *dbgc.Plan) []report.Finding {
	out := []report.Finding{}

	var orphanCount, orphanBytes int64
	samples := []string{}
	for _, r := range plan.Orphans {
		orphanCount += r.Count
		orphanBytes += r.Bytes
		if r.Count > 0 && len(samples) < 20 {
			samples = append(samples, fmt.Sprintf("%s: %d", r.Name, r.Count))
		}
	}
	orphanDesc := "孤立 message/part/blob/instruction/event/job 等"
	if orphanCount == 0 {
		orphanDesc = "孤立记录（未发现）"
	}
	out = append(out, report.Finding{
		Category:    "db-orphans",
		Description: orphanDesc,
		Count:       orphanCount,
		Bytes:       orphanBytes,
		Selected:    opts.Selection.DBOrphans || opts.Selection.Sessions || opts.Selection.Archived || opts.Selection.DBProjects,
		Samples:     samples,
	})

	// Projects without sessions.
	projectSamples := []string{}
	for _, p := range plan.Projects {
		if len(projectSamples) < 20 {
			projectSamples = append(projectSamples, p.ID+"  "+p.Worktree)
		}
	}
	projectDesc := fmt.Sprintf("无 session 且创建超过 %s 的项目", humanAge(opts.ProjectAge))
	if !opts.Selection.DBProjects {
		projectDesc += "（需 --db-projects）"
	}
	out = append(out, report.Finding{
		Category:    "db-projects",
		Description: projectDesc,
		Count:       int64(len(plan.Projects)),
		Selected:    opts.Selection.DBProjects,
		Samples:     projectSamples,
	})

	// Old sessions.
	if opts.SessionAge > 0 {
		count, bytes, messages := sumSessions(plan.Sessions)
		desc := fmt.Sprintf("超过 %s 未活动的会话（%d 条消息）", humanAge(opts.SessionAge), messages)
		if !opts.Selection.Sessions {
			desc += "（未选择，需 --sessions）"
		}
		out = append(out, report.Finding{
			Category:    "db-sessions",
			Description: desc,
			Count:       count,
			Bytes:       bytes,
			Selected:    opts.Selection.Sessions,
			Samples:     sessionSamples(plan.Sessions),
		})
	} else {
		out = append(out, report.Finding{
			Category:    "db-sessions",
			Description: "未启用（示例: --sessions 30d）",
			Selected:    false,
		})
	}

	// Archived sessions.
	if opts.ArchivedAge > 0 {
		count, bytes, messages := sumSessions(plan.Archived)
		desc := fmt.Sprintf("归档超过 %s 的会话（%d 条消息）", humanAge(opts.ArchivedAge), messages)
		if !opts.Selection.Archived {
			desc += "（未选择，需 --archived）"
		}
		out = append(out, report.Finding{
			Category:    "db-archived",
			Description: desc,
			Count:       count,
			Bytes:       bytes,
			Selected:    opts.Selection.Archived,
			Samples:     sessionSamples(plan.Archived),
		})
	} else {
		out = append(out, report.Finding{
			Category:    "db-archived",
			Description: "未启用（示例: --archived 30d）",
			Selected:    false,
		})
	}

	// Reasoning compaction.
	if opts.ReasoningAge > 0 {
		desc := fmt.Sprintf("超过 %s 的消息中的 reasoning 内容", humanAge(opts.ReasoningAge))
		if !opts.Selection.Reasoning {
			desc += "（未选择，需 --strip-reasoning）"
		}
		out = append(out, report.Finding{
			Category:    "db-reasoning",
			Description: desc,
			Count:       plan.ReasoningCount,
			Bytes:       plan.ReasoningBytes,
			Selected:    opts.Selection.Reasoning,
		})
	} else {
		out = append(out, report.Finding{
			Category:    "db-reasoning",
			Description: "未启用（示例: --strip-reasoning 30d）",
			Selected:    false,
		})
	}

	// Regenerable cache.
	cacheDesc := "可重新下载的 models-dev 缓存"
	if !opts.Selection.DBCache {
		cacheDesc += "（需 --db-cache）"
	}
	out = append(out, report.Finding{
		Category:    "db-cache",
		Description: cacheDesc,
		Count:       int64(len(plan.DBCacheKeys)),
		Bytes:       plan.DBCacheBytes,
		Selected:    opts.Selection.DBCache,
		Samples:     append([]string{}, plan.DBCacheKeys...),
	})

	// Reclaimable space.
	vacBytes := plan.Stats.FreeBytes + plan.Stats.WALSize
	vacDesc := "空闲页 + WAL，服务需停止"
	if !opts.Selection.Vacuum {
		vacDesc += "（需 --vacuum）"
	}
	out = append(out, report.Finding{
		Category:    "db-vacuum",
		Description: vacDesc,
		Count:       boolInt(vacBytes > 0),
		Bytes:       vacBytes,
		Selected:    opts.Selection.Vacuum,
		Path:        opts.Paths.DB,
	})

	return out
}

func dbUnavailableFindings(opts Options) []report.Finding {
	return []report.Finding{
		{Category: "db-orphans", Description: "数据库不可用，跳过"},
		{Category: "db-projects", Description: "数据库不可用，跳过"},
		{Category: "db-sessions", Description: "数据库不可用，跳过"},
		{Category: "db-archived", Description: "数据库不可用，跳过"},
		{Category: "db-reasoning", Description: "数据库不可用，跳过"},
		{Category: "db-cache", Description: "数据库不可用，跳过"},
		{Category: "db-vacuum", Description: "数据库不可用，跳过"},
	}
}

func filterTargets(targets []fsgc.Target, sel Selection) []fsgc.Target {
	out := make([]fsgc.Target, 0, len(targets))
	for _, t := range targets {
		if sel.ForCategory(t.Category) {
			out = append(out, t)
		}
	}
	return out
}

func extraTargets(opts Options, plan *dbgc.Plan) []fsgc.Target {
	var out []fsgc.Target
	if opts.Selection.Sessions || opts.Selection.Archived {
		ids := make([]string, 0, len(plan.Sessions)+len(plan.Archived))
		if opts.Selection.Sessions {
			for _, s := range plan.Sessions {
				ids = append(ids, s.ID)
			}
		}
		if opts.Selection.Archived {
			for _, s := range plan.Archived {
				ids = append(ids, s.ID)
			}
		}
		for _, id := range ids {
			path := filepath.Join(opts.Paths.ToolOutput, id)
			if t, ok := fsgc.MakeDirTarget("tool-output", opts.Paths.ToolOutput, path, "会话已删除"); ok {
				out = append(out, t)
			}
		}
	}
	if opts.Selection.DBProjects {
		for _, p := range plan.Projects {
			path := filepath.Join(opts.Paths.Shell, p.ID)
			if t, ok := fsgc.MakeDirTarget("shell", opts.Paths.Shell, path, "项目已删除"); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

func pickProjects(plan *dbgc.Plan, sel Selection) []dbgc.ProjectCandidate {
	if !sel.DBProjects {
		return nil
	}
	return plan.Projects
}

func planHasChanges(plan *dbgc.Plan, sel Selection) bool {
	if plan == nil {
		return false
	}
	if sel.DBOrphans {
		for _, r := range plan.Orphans {
			if r.Count > 0 {
				return true
			}
		}
	}
	if sel.DBProjects && len(plan.Projects) > 0 {
		return true
	}
	if sel.Sessions && len(plan.Sessions) > 0 {
		return true
	}
	if sel.Archived && len(plan.Archived) > 0 {
		return true
	}
	if sel.Reasoning && plan.ReasoningCount > 0 {
		return true
	}
	if sel.DBCache && len(plan.DBCacheKeys) > 0 {
		return true
	}
	return false
}

func pickSessions(list []dbgc.SessionCandidate, use bool) []dbgc.SessionCandidate {
	if !use {
		return nil
	}
	return list
}

func pickCache(plan *dbgc.Plan, sel Selection) []string {
	if !sel.DBCache {
		return nil
	}
	return plan.DBCacheKeys
}

func sumSessions(list []dbgc.SessionCandidate) (count, bytes, messages int64) {
	count = int64(len(list))
	for _, s := range list {
		bytes += s.Bytes
		messages += s.Messages
	}
	return
}

func sessionSamples(list []dbgc.SessionCandidate) []string {
	out := []string{}
	for i, s := range list {
		if i >= 20 {
			break
		}
		out = append(out, dbgc.FormatSessionSample(s))
	}
	return out
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func humanAge(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	days := d.Hours() / 24
	if days >= 1 {
		if days == float64(int64(days)) {
			return fmt.Sprintf("%d 天", int64(days))
		}
		return fmt.Sprintf("%.1f 天", days)
	}
	return d.String()
}

// logPath is kept for callers that want to display the log location.
