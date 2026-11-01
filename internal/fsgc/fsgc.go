// Package fsgc scans and removes OpenCode filesystem garbage.
package fsgc

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"opencode-gc/internal/report"
)

// RefIndex is the set of file names and relative paths referenced by session
// messages. It is used to detect shell/tool output that nothing points at.
type RefIndex struct {
	Bases map[string]struct{}
	Paths map[string]struct{}
}

// NewRefIndex returns an empty index.
func NewRefIndex() *RefIndex {
	return &RefIndex{
		Bases: make(map[string]struct{}),
		Paths: make(map[string]struct{}),
	}
}

// Add records a path from a message. The path may be absolute or relative.
func (r *RefIndex) Add(p string) {
	if r == nil {
		return
	}
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "\"'()[]{}<>,;:")
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." {
		return
	}
	r.Paths[normalize(p)] = struct{}{}
	r.Bases[normalize(pathBase(p))] = struct{}{}
}

// Has reports whether full (relative to root) is referenced.
func (r *RefIndex) Has(root, full string) bool {
	if r == nil {
		return false
	}
	if rel, err := filepath.Rel(root, full); err == nil {
		if _, ok := r.Paths[normalize(rel)]; ok {
			return true
		}
	}
	_, ok := r.Bases[normalize(filepath.Base(full))]
	return ok
}

func normalize(p string) string {
	return strings.ToLower(filepath.ToSlash(strings.TrimSpace(p)))
}

func pathBase(p string) string {
	p = filepath.ToSlash(p)
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Options controls a filesystem scan.
type Options struct {
	TmpDir        string
	LogDir        string
	ShellDir      string
	ToolOutputDir string
	ReposDir      string

	Age  time.Duration
	Now  time.Time
	Refs *RefIndex

	LiveProjects map[string]struct{}
	LiveSessions map[string]struct{}

	Verbose bool
}

// Target is one file or directory that can be deleted.
type Target struct {
	Category string
	Root     string
	Path     string
	Reason   string
	IsDir    bool
	Count    int64
	Bytes    int64
}

// Result is the outcome of a scan.
type Result struct {
	Findings []report.Finding
	Targets  []Target
}

// Scan walks the configured directories and returns everything that is garbage.
func Scan(opts Options) Result {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-opts.Age)

	var res Result
	add := func(f report.Finding) {
		if f.Samples == nil {
			f.Samples = []string{}
		}
		res.Findings = append(res.Findings, f)
	}

	// tmp
	tmp, err := scanFlat(opts.TmpDir, "tmp", cutoff, opts.Verbose)
	if err != nil {
		tmp.Finding.Error = err.Error()
	}
	tmp.Finding.Description = fmt.Sprintf("%s 中超过 %s 的文件", opts.TmpDir, humanAge(opts.Age))
	add(tmp.Finding)
	res.Targets = append(res.Targets, tmp.Targets...)

	// logs
	logs, err := scanFlat(opts.LogDir, "logs", cutoff, opts.Verbose)
	if err != nil {
		logs.Finding.Error = err.Error()
	}
	logs.Finding.Description = fmt.Sprintf("log 目录中超过 %s 的日志", humanAge(opts.Age))
	add(logs.Finding)
	res.Targets = append(res.Targets, logs.Targets...)

	// shell
	shell := scanReferenced(opts.ShellDir, "shell", opts.LiveProjects, opts.Refs, cutoff, opts.Verbose)
	shell.Finding.Description = fmt.Sprintf("未被消息引用或项目已不存在的 shell 输出（超过 %s）", humanAge(opts.Age))
	add(shell.Finding)
	res.Targets = append(res.Targets, shell.Targets...)

	// tool-output
	tool := scanReferenced(opts.ToolOutputDir, "tool-output", opts.LiveSessions, opts.Refs, cutoff, opts.Verbose)
	tool.Finding.Description = fmt.Sprintf("未被消息引用或会话已不存在的 tool-output（超过 %s）", humanAge(opts.Age))
	add(tool.Finding)
	res.Targets = append(res.Targets, tool.Targets...)

	// repos
	repos := scanRepos(opts.ReposDir, opts.LiveProjects, opts.LiveSessions, cutoff, opts.Verbose)
	repos.Finding.Description = fmt.Sprintf("repos 中无对应项目/会话且超过 %s 的目录", humanAge(opts.Age))
	add(repos.Finding)
	res.Targets = append(res.Targets, repos.Targets...)

	return res
}

type categoryScan struct {
	Finding report.Finding
	Targets []Target
}

func (c *categoryScan) add(t Target) {
	c.Targets = append(c.Targets, t)
	c.Finding.Count++
	c.Finding.Bytes += t.Bytes
	if len(c.Finding.Samples) < 20 {
		c.Finding.Samples = append(c.Finding.Samples, t.Path)
	}
}

func scanFlat(root, category string, cutoff time.Time, verbose bool) (categoryScan, error) {
	out := categoryScan{Finding: report.Finding{Category: category, Path: root, Samples: []string{}}}
	if root == "" {
		return out, nil
	}
	st, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	if !st.IsDir() {
		return out, fmt.Errorf("%s is not a directory", root)
	}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		out.add(Target{
			Category: category,
			Root:     root,
			Path:     path,
			IsDir:    false,
			Count:    1,
			Bytes:    sizeOf(info),
			Reason:   "超过时间阈值",
		})
		return nil
	})
	if err != nil {
		out.Finding.Error = err.Error()
	}
	return out, nil
}

func scanReferenced(root, category string, live map[string]struct{}, refs *RefIndex, cutoff time.Time, verbose bool) categoryScan {
	out := categoryScan{Finding: report.Finding{Category: category, Path: root, Samples: []string{}}}
	if root == "" {
		return out
	}
	if live == nil || refs == nil {
		out.Finding.Error = "数据库不可用，跳过引用检查"
		return out
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			out.Finding.Error = err.Error()
		}
		return out
	}

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		parts := strings.SplitN(relSlash, "/", 2)
		nested := len(parts) == 2
		liveContainer := true // flat tool-output files have no container
		if nested {
			_, liveContainer = live[parts[0]]
		}
		referenced := refs.Has(root, path)
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		if liveContainer && referenced {
			return nil
		}
		reason := "未被消息引用"
		if !liveContainer {
			reason = "项目/会话已不存在"
		}
		out.add(Target{
			Category: category,
			Root:     root,
			Path:     path,
			IsDir:    false,
			Count:    1,
			Bytes:    sizeOf(info),
			Reason:   reason,
		})
		return nil
	})
	return out
}

func scanRepos(root string, projects, sessions map[string]struct{}, cutoff time.Time, verbose bool) categoryScan {
	out := categoryScan{Finding: report.Finding{Category: "repos", Path: root, Samples: []string{}}}
	if root == "" {
		return out
	}
	if projects == nil || sessions == nil {
		out.Finding.Error = "数据库不可用，跳过"
		return out
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			out.Finding.Error = err.Error()
		}
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if _, ok := projects[name]; ok {
			continue
		}
		if _, ok := sessions[name]; ok {
			continue
		}
		path := filepath.Join(root, name)
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if info.IsDir() {
			count, bytes := treeStats(path)
			out.add(Target{
				Category: "repos",
				Root:     root,
				Path:     path,
				IsDir:    true,
				Count:    count,
				Bytes:    bytes,
				Reason:   "无对应项目/会话",
			})
		} else {
			out.add(Target{
				Category: "repos",
				Root:     root,
				Path:     path,
				Count:    1,
				Bytes:    sizeOf(info),
				Reason:   "无对应项目/会话",
			})
		}
	}
	return out
}

// MakeDirTarget builds a recursive directory target, usually for output that
// belonged to a session or project that is being deleted.
func MakeDirTarget(category, root, path, reason string) (Target, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return Target{}, false
	}
	if !within(root, path) {
		return Target{}, false
	}
	count, bytes := treeStats(path)
	return Target{
		Category: category,
		Root:     root,
		Path:     path,
		IsDir:    true,
		Count:    count,
		Bytes:    bytes,
		Reason:   reason,
	}, true
}

// DeleteResult is the outcome of a delete pass.
type DeleteResult struct {
	Deleted int64
	Bytes   int64
	Errors  []string
}

// Delete removes the targets, then prunes empty directories older than cutoff.
func Delete(targets []Target, cutoff time.Time) DeleteResult {
	var out DeleteResult
	roots := make(map[string]struct{})

	sort.SliceStable(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if a.IsDir != b.IsDir {
			return !a.IsDir
		}
		if a.IsDir {
			return depth(a.Path) > depth(b.Path)
		}
		return a.Path < b.Path
	})

	for _, t := range targets {
		if t.Root != "" {
			roots[t.Root] = struct{}{}
		}
		if !within(t.Root, t.Path) {
			out.Errors = append(out.Errors, fmt.Sprintf("拒绝删除目录外路径: %s", t.Path))
			continue
		}
		info, err := os.Lstat(t.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", t.Path, err))
			continue
		}
		if info.IsDir() && !t.IsDir {
			// Only empty directories are targeted this way.
			if err := os.Remove(t.Path); err != nil {
				if !os.IsNotExist(err) {
					out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", t.Path, err))
				}
				continue
			}
		} else {
			if err := os.Remove(t.Path); err != nil {
				if !os.IsNotExist(err) {
					out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", t.Path, err))
				}
				continue
			}
		}
		out.Deleted += t.Count
		if t.Count == 0 {
			out.Deleted++
		}
		out.Bytes += t.Bytes
	}

	for root := range roots {
		pruneEmpty(root, cutoff)
	}
	return out
}

func pruneEmpty(root string, cutoff time.Time) {
	if root == "" {
		return
	}
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root || !d.IsDir() {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return depth(dirs[i]) > depth(dirs[j]) })
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(dir)
	}
}

func within(root, path string) bool {
	if root == "" {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

func treeStats(root string) (count, bytes int64) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			count++
			bytes += sizeOf(info)
		}
		return nil
	})
	return count, bytes
}

func sizeOf(info fs.FileInfo) int64 {
	if info == nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

func depth(p string) int {
	return strings.Count(filepath.Clean(p), string(filepath.Separator))
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
