package report

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// SessionRow is one session in the analysis/delete reports.
type SessionRow struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	ProjectID string `json:"projectId"`
	Project   string `json:"project"`
	Worktree  string `json:"worktree"`
	Directory string `json:"directory"`
	ParentID  string `json:"parentId,omitempty"`

	Created  int64 `json:"created"`
	Updated  int64 `json:"updated"`
	Archived int64 `json:"archived,omitempty"`

	Messages         int64   `json:"messages"`
	MessageBytes     int64   `json:"messageBytes"`
	BlobBytes        int64   `json:"blobBytes"`
	PartBytes        int64   `json:"partBytes"`
	InstructionBytes int64   `json:"instructionBytes"`
	TotalBytes       int64   `json:"totalBytes"`
	Tokens           int64   `json:"tokens"`
	Cost             float64 `json:"cost"`
	Children         int64   `json:"children"`

	Protected bool `json:"protected"`
	Recent    bool `json:"recent"`
}

// SessionList is the result of `sessions list`.
type SessionList struct {
	Tool    string `json:"tool"`
	Version string `json:"version"`
	Command string `json:"command"`

	DataDir string `json:"dataDir"`
	DBPath  string `json:"dbPath"`

	Sort          string `json:"sort"`
	Limit         int    `json:"limit"`
	TotalSessions int    `json:"totalSessions"`
	TotalBytes    int64  `json:"totalBytes"`
	Shown         int    `json:"shown"`
	ShownBytes    int64  `json:"shownBytes"`

	Rows     []SessionRow `json:"sessions"`
	Warnings []string     `json:"warnings"`
	Errors   []string     `json:"errors"`

	Verbose  bool `json:"-"`
	ExitCode int  `json:"-"`
}

// SkippedSession explains why a requested session was not deleted.
type SkippedSession struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// SessionDeleteReport is the result of `sessions delete`.
type SessionDeleteReport struct {
	SessionList

	Applied         bool             `json:"applied"`
	Force           bool             `json:"force"`
	WithChildren    bool             `json:"withChildren"`
	DeletedSessions int64            `json:"deletedSessions"`
	DBRows          int64            `json:"dbRows"`
	BackupPath      string           `json:"backupPath,omitempty"`
	BackupBytes     int64            `json:"backupBytes,omitempty"`
	VacuumDone      bool             `json:"vacuumDone"`
	DBSizeBefore    int64            `json:"dbSizeBefore"`
	DBSizeAfter     int64            `json:"dbSizeAfter"`
	WALSizeBefore   int64            `json:"walSizeBefore"`
	WALSizeAfter    int64            `json:"walSizeAfter"`
	Skipped         []SkippedSession `json:"skipped"`
}

// SortLabel renders the sort key for humans.
func SortLabel(sortKey string) string {
	switch sortKey {
	case "messages":
		return "消息数量"
	case "updated":
		return "最后活动时间"
	case "created":
		return "创建时间"
	case "cost":
		return "费用"
	case "tokens":
		return "Token 数"
	case "size", "":
		return "总大小"
	default:
		return sortKey
	}
}

// PrintSessionList writes the session analysis table.
func PrintSessionList(w io.Writer, l SessionList) {
	verb := "会话分析"
	if l.Command == "sessions delete" {
		verb = "会话清理"
	}
	fmt.Fprintf(w, "%s %s — %s\n", l.Tool, l.Version, verb)
	fmt.Fprintf(w, "  data  %s\n", l.DataDir)
	fmt.Fprintf(w, "  db    %s\n", l.DBPath)
	fmt.Fprintf(w, "  排序  %s（%s），显示 %d/%d\n", l.Sort, SortLabel(l.Sort), l.Shown, l.TotalSessions)
	fmt.Fprintln(w)
	if len(l.Rows) > 0 {
		printSessionRows(w, l.Rows, l.Verbose)
	} else {
		fmt.Fprintln(w, "  没有匹配的会话。")
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  共 %d 个会话，显示 %d 个；总计 %s，显示 %s\n",
		l.TotalSessions, l.Shown, HumanBytes(l.TotalBytes), HumanBytes(l.ShownBytes))
	printNotes(w, l.Warnings, l.Errors)
}

// PrintSessionDelete writes the targeted deletion report.
func PrintSessionDelete(w io.Writer, d SessionDeleteReport) {
	verb := "会话清理预览"
	if d.Applied {
		verb = "会话清理"
	}
	fmt.Fprintf(w, "%s %s — %s\n", d.Tool, d.Version, verb)
	fmt.Fprintf(w, "  data  %s\n", d.DataDir)
	fmt.Fprintf(w, "  db    %s\n", d.DBPath)
	fmt.Fprintf(w, "  待删除 %d 个会话，共 %s\n", len(d.Rows), HumanBytes(d.TotalBytes))
	if d.Force {
		fmt.Fprintln(w, "  --force: 允许删除 24h 内更新过的会话（受保护会话仍然不会删除）")
	}
	if d.WithChildren {
		fmt.Fprintln(w, "  --with-children: 子会话会一并删除")
	}
	fmt.Fprintln(w)
	if len(d.Rows) > 0 {
		printSessionRows(w, d.Rows, d.Verbose)
	} else {
		fmt.Fprintln(w, "  没有可删除的会话。")
	}

	if len(d.Skipped) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  跳过:")
		for _, s := range d.Skipped {
			fmt.Fprintf(w, "    - %s: %s\n", s.ID, s.Reason)
		}
	}

	if d.Applied {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  已删除会话: %d 个\n", d.DeletedSessions)
		if d.DBRows > 0 {
			fmt.Fprintf(w, "  数据库修改: %d 行\n", d.DBRows)
			fmt.Fprintf(w, "  数据库大小: %s -> %s\n", HumanBytes(d.DBSizeBefore), HumanBytes(d.DBSizeAfter))
			fmt.Fprintf(w, "  WAL 大小:   %s -> %s\n", HumanBytes(d.WALSizeBefore), HumanBytes(d.WALSizeAfter))
		}
		if d.VacuumDone {
			fmt.Fprintln(w, "  已执行 WAL checkpoint + VACUUM。")
		}
		if d.BackupPath != "" {
			fmt.Fprintf(w, "  备份: %s（%s）\n", d.BackupPath, HumanBytes(d.BackupBytes))
		}
	} else if len(d.Rows) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  真正执行请加 --apply。")
	}

	printNotes(w, d.Warnings, d.Errors)
}

func printSessionRows(w io.Writer, rows []SessionRow, verbose bool) {
	fmt.Fprintf(w, "  %-30s %10s %7s %16s  %s\n", "ID", "大小", "消息", "更新时间", "项目")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("-", 86))
	for _, r := range rows {
		flag := "  "
		switch {
		case r.Protected && r.Recent:
			flag = "*!"
		case r.Protected:
			flag = "* "
		case r.Recent:
			flag = "! "
		}
		project := r.Project
		if project == "" {
			project = r.Worktree
		}
		fmt.Fprintf(w, "%s %-30s %10s %7d %16s  %s\n",
			flag, r.ID, HumanBytes(r.TotalBytes), r.Messages, formatMillis(r.Updated), truncateRunes(project, 48))
		if verbose {
			fmt.Fprintf(w, "     slug=%s title=%s\n", r.Slug, truncateRunes(r.Title, 60))
			fmt.Fprintf(w, "     parent=%s children=%d tokens=%d cost=%.4f\n", r.ParentID, r.Children, r.Tokens, r.Cost)
			fmt.Fprintf(w, "     dir=%s\n", r.Directory)
			fmt.Fprintf(w, "     message=%s blob=%s part=%s instruction=%s\n",
				HumanBytes(r.MessageBytes), HumanBytes(r.BlobBytes), HumanBytes(r.PartBytes), HumanBytes(r.InstructionBytes))
		}
	}
}

func printNotes(w io.Writer, warnings, errors []string) {
	if len(warnings) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  提示:")
		for _, m := range warnings {
			fmt.Fprintf(w, "    - %s\n", m)
		}
	}
	if len(errors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  错误:")
		for _, m := range errors {
			fmt.Fprintf(w, "    - %s\n", m)
		}
	}
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04")
}

func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
