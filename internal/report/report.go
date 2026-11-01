// Package report defines the scan/clean result model and its text and JSON
// renderers.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Finding is one cleanup rule and everything it found.
type Finding struct {
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Count       int64    `json:"count"`
	Bytes       int64    `json:"bytes"`
	Selected    bool     `json:"selected"`
	Path        string   `json:"path,omitempty"`
	Samples     []string `json:"samples,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// Report is the complete result of a scan or clean run.
type Report struct {
	Tool    string `json:"tool"`
	Version string `json:"version"`
	Command string `json:"command"`

	DataDir string `json:"dataDir"`
	TmpDir  string `json:"tmpDir"`
	DBPath  string `json:"dbPath"`

	DBExists bool  `json:"dbExists"`
	DBSize   int64 `json:"dbSize"`
	WALSize  int64 `json:"walSize"`
	FreeSize int64 `json:"freeSize"`

	Findings []Finding `json:"findings"`
	Warnings []string  `json:"warnings"`
	Errors   []string  `json:"errors"`

	Applied        bool  `json:"applied"`
	DeletedFiles   int64 `json:"deletedFiles"`
	FreedFileBytes int64 `json:"freedFileBytes"`
	DBRows         int64 `json:"dbRows"`
	DBChanged      bool  `json:"dbChanged"`
	VacuumDone     bool  `json:"vacuumDone"`

	DBSizeBefore  int64 `json:"dbSizeBefore"`
	DBSizeAfter   int64 `json:"dbSizeAfter"`
	WALSizeBefore int64 `json:"walSizeBefore"`
	WALSizeAfter  int64 `json:"walSizeAfter"`

	BackupPath  string `json:"backupPath,omitempty"`
	BackupBytes int64  `json:"backupBytes,omitempty"`

	Verbose bool `json:"-"`

	ExitCode int `json:"-"`
}

// HumanBytes renders a byte count using binary units.
func HumanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Print writes the human readable report.
func Print(w io.Writer, r Report) {
	command := r.Command
	if command == "" {
		command = "scan"
	}
	verb := "扫描"
	switch command {
	case "backup":
		verb = "备份"
	case "vacuum":
		verb = "空间回收"
	case "clean":
		if r.Applied {
			verb = "清理"
		} else {
			verb = "预览"
		}
	default:
		if r.Applied {
			verb = "清理"
		}
	}
	fmt.Fprintf(w, "%s %s — %s\n", r.Tool, r.Version, verb)
	fmt.Fprintf(w, "  data  %s\n", r.DataDir)
	fmt.Fprintf(w, "  tmp   %s\n", r.TmpDir)
	if r.DBExists {
		extra := fmt.Sprintf("WAL %s", HumanBytes(r.WALSize))
		if r.FreeSize > 0 {
			extra += fmt.Sprintf("，空闲页 %s", HumanBytes(r.FreeSize))
		}
		fmt.Fprintf(w, "  db    %s  (%s，%s)\n", r.DBPath, HumanBytes(r.DBSize), extra)
	} else {
		fmt.Fprintf(w, "  db    %s  (不存在)\n", r.DBPath)
	}
	fmt.Fprintln(w)

	if len(r.Findings) > 0 {
		fmt.Fprintf(w, "  %-14s %7s %10s  %s\n", "类别", "数量", "大小", "说明")
		fmt.Fprintf(w, "  %s\n", strings.Repeat("-", 78))
		for _, f := range r.Findings {
			mark := " "
			if f.Selected {
				mark = "*"
			}
			fmt.Fprintf(w, "%s %-14s %7d %10s  %s\n", mark, f.Category, f.Count, HumanBytes(f.Bytes), f.Description)
			if f.Error != "" {
				fmt.Fprintf(w, "    ! %s\n", f.Error)
			}
			if r.Verbose && len(f.Samples) > 0 {
				for _, s := range f.Samples {
					fmt.Fprintf(w, "    - %s\n", s)
				}
			}
		}

		var fileBytes, dbBytes, vacuumBytes int64
		var fileSelected, dbSelected, vacuumSelected bool
		for _, f := range r.Findings {
			if !f.Selected {
				continue
			}
			switch {
			case f.Category == "db-vacuum":
				vacuumBytes += f.Bytes
				vacuumSelected = true
			case strings.HasPrefix(f.Category, "db-"):
				dbBytes += f.Bytes
				dbSelected = true
			default:
				fileBytes += f.Bytes
				fileSelected = true
			}
		}

		parts := []string{}
		if fileSelected {
			parts = append(parts, "文件 "+HumanBytes(fileBytes))
		}
		if dbSelected {
			parts = append(parts, "数据库记录 "+HumanBytes(dbBytes))
		}
		if vacuumSelected {
			parts = append(parts, "空间回收 "+HumanBytes(vacuumBytes))
		}
		fmt.Fprintln(w)
		if len(parts) > 0 {
			label := "当前选择可回收"
			if r.Applied {
				label = "本次选择"
			}
			fmt.Fprintf(w, "  %s: %s\n", label, strings.Join(parts, " + "))
		} else {
			fmt.Fprintln(w, "  当前没有选择任何清理类别。")
		}
	}

	if r.Applied {
		fmt.Fprintln(w)
		if r.DeletedFiles > 0 {
			fmt.Fprintf(w, "  已删除文件: %d 个（%s）\n", r.DeletedFiles, HumanBytes(r.FreedFileBytes))
		}
		if r.DBChanged {
			fmt.Fprintf(w, "  数据库修改: %d 行\n", r.DBRows)
			fmt.Fprintf(w, "  数据库大小: %s -> %s\n", HumanBytes(r.DBSizeBefore), HumanBytes(r.DBSizeAfter))
			fmt.Fprintf(w, "  WAL 大小:   %s -> %s\n", HumanBytes(r.WALSizeBefore), HumanBytes(r.WALSizeAfter))
		}
		if r.Command == "vacuum" {
			fmt.Fprintf(w, "  数据库大小: %s -> %s\n", HumanBytes(r.DBSizeBefore), HumanBytes(r.DBSizeAfter))
			fmt.Fprintf(w, "  WAL 大小:   %s -> %s\n", HumanBytes(r.WALSizeBefore), HumanBytes(r.WALSizeAfter))
		}
		if r.VacuumDone {
			fmt.Fprintln(w, "  已执行 WAL checkpoint + VACUUM。")
		}
		if r.BackupPath != "" {
			fmt.Fprintf(w, "  备份: %s（%s）\n", r.BackupPath, HumanBytes(r.BackupBytes))
		}
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  提示:")
		for _, m := range r.Warnings {
			fmt.Fprintf(w, "    - %s\n", m)
		}
	}
	if len(r.Errors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  错误:")
		for _, m := range r.Errors {
			fmt.Fprintf(w, "    - %s\n", m)
		}
	}

	if !r.Applied && (r.Command == "" || r.Command == "scan" || r.Command == "clean") {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  * = 当前选择；执行清理请加 --apply。先用 `opencode-gc clean --all` 预览全部安全类别。\n")
	}
	if !r.Applied && r.Command == "vacuum" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  执行空间回收请加 --apply（需要先停止 OpenCode 服务）。")
	}
}

// WriteJSON writes the machine readable report.
func WriteJSON(w io.Writer, r Report) error {
	if r.Findings == nil {
		r.Findings = []Finding{}
	}
	for i := range r.Findings {
		if r.Findings[i].Samples == nil {
			r.Findings[i].Samples = []string{}
		}
	}
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	if r.Errors == nil {
		r.Errors = []string{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
