package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/alexflint/go-arg"

	"opencode-gc/internal/age"
	"opencode-gc/internal/gc"
	"opencode-gc/internal/paths"
	"opencode-gc/internal/report"
)

const version = "0.3.0"

type pathOpts struct {
	DataDir  string `arg:"--data-dir" placeholder:"PATH" help:"覆盖 OpenCode data 目录"`
	TmpDir   string `arg:"--tmp-dir" placeholder:"PATH" help:"覆盖 OpenCode tmp 目录"`
	DBPath   string `arg:"--db" placeholder:"PATH" help:"覆盖 opencode.db 路径"`
	JSON     bool   `arg:"--json" help:"输出 JSON"`
	Verbose  bool   `arg:"-v,--verbose" help:"显示更多明细"`
	NoDetect bool   `arg:"--no-detect" help:"不调用 opencode debug paths，直接使用默认路径"`
}

type fileOpts struct {
	Age       age.Duration `arg:"--age" placeholder:"DUR" help:"文件垃圾时间阈值，如 7d、36h（默认 7d；设置 --older-than 时跟随它）"`
	OlderThan age.Duration `arg:"--older-than" placeholder:"DUR" help:"统一时间范围：启用并清理超过该时长的文件、项目、旧会话、归档会话和 reasoning；显式类别参数可缩小范围"`
}

type dbOpts struct {
	ProjectAge   age.Duration `arg:"--project-age" placeholder:"DUR" help:"无 session 项目的判定年龄（默认 30d；设置 --older-than 时跟随它）"`
	Sessions     age.Duration `arg:"--sessions" placeholder:"DUR" help:"删除超过该时间未活动的会话，如 30d（默认关闭；设置 --older-than 时跟随它）"`
	Archived     age.Duration `arg:"--archived" placeholder:"DUR" help:"删除归档超过该时间的会话（默认关闭；设置 --older-than 时跟随它）"`
	Reasoning    age.Duration `arg:"--strip-reasoning" placeholder:"DUR" help:"删除旧消息中的 reasoning 内容（默认关闭；设置 --older-than 时跟随它）"`
	DBProjects   bool         `arg:"--db-projects" help:"删除无 session 且超过 --project-age 的项目"`
	DBCache      bool         `arg:"--db-cache" help:"删除可重新下载的 models-dev 缓存"`
	AllowRunning bool         `arg:"--allow-running" help:"服务运行时仍允许写库（不推荐）"`
}

type scanCmd struct {
	pathOpts
	fileOpts
	dbOpts
}

type cleanCmd struct {
	pathOpts
	fileOpts
	dbOpts

	Apply      bool   `arg:"--apply" help:"真正执行删除；不带时只预览"`
	All        bool   `arg:"--all" help:"清理所有非历史垃圾（tmp/logs/shell/tool-output/repos/DB 孤立记录/无会话项目/DB 缓存 + VACUUM）；历史需 --older-than 或显式 flags"`
	Tmp        bool   `arg:"--tmp" help:"清理 <tmp>/opencode"`
	Logs       bool   `arg:"--logs" help:"清理旧日志"`
	Shell      bool   `arg:"--shell" help:"清理孤立 shell 输出"`
	ToolOutput bool   `arg:"--tool-output" help:"清理孤立 tool-output"`
	Repos      bool   `arg:"--repos" help:"清理无对应项目的 repos 目录"`
	DBOrphans  bool   `arg:"--db-orphans" help:"清理数据库孤立记录"`
	Vacuum     bool   `arg:"--vacuum" help:"清理后执行 checkpoint + VACUUM"`
	NoBackup   bool   `arg:"--no-backup" help:"写库前不自动备份（不推荐）"`
	BackupDir  string `arg:"--backup-dir" placeholder:"PATH" help:"备份目录"`
}

type vacuumCmd struct {
	pathOpts
	Apply bool `arg:"--apply" help:"真正执行 VACUUM；不带时只预览"`
}

type backupCmd struct {
	pathOpts
	Out string `arg:"--out" placeholder:"PATH" help:"备份文件路径"`
}

type sessionsListCmd struct {
	pathOpts
	Sort      string       `arg:"--sort" default:"size" placeholder:"FIELD" help:"排序字段：size、messages、updated、created、cost、tokens"`
	Limit     int          `arg:"--limit" default:"20" help:"显示条数，0 表示全部"`
	Project   string       `arg:"--project" placeholder:"TEXT" help:"按项目名/worktree/directory/session 字段过滤"`
	OlderThan age.Duration `arg:"--older-than" placeholder:"DUR" help:"只显示超过该时间未活动的会话"`
}

type sessionsDeleteCmd struct {
	pathOpts
	IDs          []string `arg:"positional,required" placeholder:"SESSION_ID" help:"要删除的 session ID，可写多个"`
	Apply        bool     `arg:"--apply" help:"真正执行删除；不带时只预览"`
	Force        bool     `arg:"--force" help:"允许删除 24h 内更新过的会话（受保护会话仍不会删除）"`
	WithChildren bool     `arg:"--with-children" help:"连同子会话一起删除"`
	Vacuum       bool     `arg:"--vacuum" help:"删除后执行 checkpoint + VACUUM"`
	NoBackup     bool     `arg:"--no-backup" help:"写库前不自动备份（不推荐）"`
	BackupDir    string   `arg:"--backup-dir" placeholder:"PATH" help:"备份目录"`
	AllowRunning bool     `arg:"--allow-running" help:"服务运行时仍允许写库（不推荐）"`
}

type sessionsCmd struct {
	List   *sessionsListCmd   `arg:"subcommand:list" help:"列出会话并按大小等字段排序"`
	Delete *sessionsDeleteCmd `arg:"subcommand:delete" help:"按 session ID 定点删除会话"`
}

type rootArgs struct {
	Scan     *scanCmd     `arg:"subcommand:scan" help:"扫描并报告垃圾（默认命令）"`
	Clean    *cleanCmd    `arg:"subcommand:clean" help:"清理垃圾；默认只预览，--apply 执行"`
	Sessions *sessionsCmd `arg:"subcommand:sessions" help:"分析会话大小、按 ID 定点删除会话"`
	Vacuum   *vacuumCmd   `arg:"subcommand:vacuum" help:"回收数据库空间（checkpoint + VACUUM）"`
	Backup   *backupCmd   `arg:"subcommand:backup" help:"创建一致的数据库备份"`
}

func (rootArgs) Version() string { return "opencode-gc " + version }

func main() {
	// `opencode-gc` with no arguments is the read-only scan command.
	if len(os.Args) == 1 {
		os.Args = append(os.Args, "scan")
	}

	var args rootArgs
	arg.MustParse(&args)

	ctx := context.Background()
	var rep report.Report

	switch {
	case args.Scan != nil:
		opts := buildOptions("scan", args.Scan.pathOpts, args.Scan.fileOpts, args.Scan.dbOpts, "")
		opts.Selection = scanSelection(args.Scan.fileOpts, args.Scan.dbOpts)
		limitAgesToSelection(&opts)
		rep = gc.Run(ctx, opts)

	case args.Clean != nil:
		opts := buildOptions("clean", args.Clean.pathOpts, args.Clean.fileOpts, args.Clean.dbOpts, args.Clean.BackupDir)
		opts.Selection = cleanSelection(args.Clean)
		limitAgesToSelection(&opts)
		opts.Apply = args.Clean.Apply
		opts.Backup = !args.Clean.NoBackup
		opts.BackupKeep = 3
		rep = gc.Run(ctx, opts)

	case args.Sessions != nil:
		runSessions(ctx, args.Sessions)
		return

	case args.Vacuum != nil:
		opts := buildOptions("vacuum", args.Vacuum.pathOpts, fileOpts{}, dbOpts{}, "")
		opts.Apply = args.Vacuum.Apply
		opts.Selection = gc.Selection{Vacuum: true}
		rep = gc.Run(ctx, opts)

	case args.Backup != nil:
		opts := buildOptions("backup", args.Backup.pathOpts, fileOpts{}, dbOpts{}, "")
		opts.BackupOut = args.Backup.Out
		opts.Apply = true
		rep = gc.Run(ctx, opts)

	default:
		fmt.Fprintln(os.Stderr, "请指定命令：scan、clean、vacuum 或 backup")
		os.Exit(2)
	}

	if args.JSONEnabled() {
		if err := report.WriteJSON(os.Stdout, rep); err != nil {
			fmt.Fprintln(os.Stderr, "输出 JSON 失败:", err)
			os.Exit(1)
		}
	} else {
		report.Print(os.Stdout, rep)
	}

	if rep.ExitCode != 0 {
		os.Exit(rep.ExitCode)
	}
}

func (r rootArgs) JSONEnabled() bool {
	switch {
	case r.Scan != nil:
		return r.Scan.JSON
	case r.Clean != nil:
		return r.Clean.JSON
	case r.Vacuum != nil:
		return r.Vacuum.JSON
	case r.Backup != nil:
		return r.Backup.JSON
	case r.Sessions != nil:
		if r.Sessions.List != nil {
			return r.Sessions.List.JSON
		}
		if r.Sessions.Delete != nil {
			return r.Sessions.Delete.JSON
		}
	}
	return false
}

func runSessions(ctx context.Context, cmd *sessionsCmd) {
	if cmd == nil {
		return
	}
	if cmd.Delete != nil {
		d := cmd.Delete
		opts := gc.SessionDeleteOptions{
			Version:      version,
			Paths:        resolvePaths(d.pathOpts, d.BackupDir),
			Now:          time.Now(),
			IDs:          d.IDs,
			Apply:        d.Apply,
			Force:        d.Force,
			WithChildren: d.WithChildren,
			Vacuum:       d.Vacuum,
			Backup:       !d.NoBackup,
			BackupKeep:   3,
			AllowRunning: d.AllowRunning,
			Verbose:      d.Verbose,
		}
		res := gc.DeleteSessions(ctx, opts)
		if d.JSON {
			if err := report.WriteJSONValue(os.Stdout, res); err != nil {
				fmt.Fprintln(os.Stderr, "输出 JSON 失败:", err)
				os.Exit(1)
			}
		} else {
			report.PrintSessionDelete(os.Stdout, res)
		}
		if res.ExitCode != 0 {
			os.Exit(res.ExitCode)
		}
		return
	}

	l := cmd.List
	if l == nil {
		l = &sessionsListCmd{Sort: "size", Limit: 20}
	}
	opts := gc.SessionListOptions{
		Version:   version,
		Paths:     resolvePaths(l.pathOpts, ""),
		Now:       time.Now(),
		Sort:      l.Sort,
		Limit:     l.Limit,
		Project:   l.Project,
		OlderThan: l.OlderThan.Duration,
		Verbose:   l.Verbose,
	}
	res := gc.ListSessions(ctx, opts)
	if l.JSON {
		if err := report.WriteJSONValue(os.Stdout, res); err != nil {
			fmt.Fprintln(os.Stderr, "输出 JSON 失败:", err)
			os.Exit(1)
		}
	} else {
		report.PrintSessionList(os.Stdout, res)
	}
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
}

func resolvePaths(po pathOpts, backupDir string) paths.Paths {
	p := paths.Resolve(context.Background(), !po.NoDetect)
	p.Apply(po.DataDir, po.TmpDir, po.DBPath, backupDir)
	return p
}

func buildOptions(command string, po pathOpts, fo fileOpts, dbo dbOpts, backupDir string) gc.Options {
	p := resolvePaths(po, backupDir)
	ages := resolveAges(fo, dbo)

	return gc.Options{
		Command:      command,
		Version:      version,
		Paths:        p,
		Now:          time.Now(),
		FileAge:      ages.file,
		ProjectAge:   ages.project,
		SessionAge:   ages.session,
		ArchivedAge:  ages.archived,
		ReasoningAge: ages.reasoning,
		AllowRunning: dbo.AllowRunning,
		Verbose:      po.Verbose,
	}
}

type resolvedAges struct {
	file      time.Duration
	project   time.Duration
	session   time.Duration
	archived  time.Duration
	reasoning time.Duration
}

// resolveAges applies --older-than as a fallback and the historical defaults.
func resolveAges(fo fileOpts, dbo dbOpts) resolvedAges {
	older := fo.OlderThan.Duration
	a := resolvedAges{
		file:      fo.Age.Duration,
		project:   dbo.ProjectAge.Duration,
		session:   dbo.Sessions.Duration,
		archived:  dbo.Archived.Duration,
		reasoning: dbo.Reasoning.Duration,
	}
	if a.file == 0 {
		if older > 0 {
			a.file = older
		} else {
			a.file = 7 * 24 * time.Hour
		}
	}
	if a.project == 0 {
		if older > 0 {
			a.project = older
		} else {
			a.project = 30 * 24 * time.Hour
		}
	}
	if older > 0 {
		if a.session == 0 {
			a.session = older
		}
		if a.archived == 0 {
			a.archived = older
		}
		if a.reasoning == 0 {
			a.reasoning = older
		}
	}
	return a
}

// limitAgesToSelection avoids scanning expensive categories that were narrowed
// away by explicit flags.
func limitAgesToSelection(opts *gc.Options) {
	if !opts.Selection.Sessions {
		opts.SessionAge = 0
	}
	if !opts.Selection.Archived {
		opts.ArchivedAge = 0
	}
	if !opts.Selection.Reasoning {
		opts.ReasoningAge = 0
	}
}

func scanSelection(fo fileOpts, dbo dbOpts) gc.Selection {
	sel := gc.DefaultSelection()
	if fo.OlderThan.Duration > 0 {
		sel = gc.UnifiedSelection()
	}
	if dbo.DBProjects {
		sel.DBProjects = true
	}
	if dbo.DBCache {
		sel.DBCache = true
	}
	if dbo.Sessions.Duration > 0 {
		sel.Sessions = true
	}
	if dbo.Archived.Duration > 0 {
		sel.Archived = true
	}
	if dbo.Reasoning.Duration > 0 {
		sel.Reasoning = true
	}
	return sel
}

func cleanSelection(c *cleanCmd) gc.Selection {
	explicit := c.Tmp || c.Logs || c.Shell || c.ToolOutput || c.Repos ||
		c.DBOrphans || c.DBProjects || c.DBCache || c.Vacuum ||
		c.Sessions.Duration > 0 || c.Archived.Duration > 0 || c.Reasoning.Duration > 0
	older := c.OlderThan.Duration > 0

	var sel gc.Selection
	switch {
	case c.All:
		sel = gc.AllSelection()
	case older && !explicit:
		sel = gc.UnifiedSelection()
	case !explicit:
		sel = gc.DefaultSelection()
	}

	if c.Tmp {
		sel.Tmp = true
	}
	if c.Logs {
		sel.Logs = true
	}
	if c.Shell {
		sel.Shell = true
	}
	if c.ToolOutput {
		sel.ToolOutput = true
	}
	if c.Repos {
		sel.Repos = true
	}
	if c.DBOrphans {
		sel.DBOrphans = true
	}
	if c.DBProjects {
		sel.DBProjects = true
	}
	if c.DBCache {
		sel.DBCache = true
	}
	if c.Sessions.Duration > 0 {
		sel.Sessions = true
	}
	if c.Archived.Duration > 0 {
		sel.Archived = true
	}
	if c.Reasoning.Duration > 0 {
		sel.Reasoning = true
	}
	if c.Vacuum {
		sel.Vacuum = true
	}
	// --all is additive: combining it with --older-than must not shrink the
	// scope back to non-history garbage.
	if older && c.All {
		sel.Sessions = true
		sel.Archived = true
		sel.Reasoning = true
	}
	return sel
}
