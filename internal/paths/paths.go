// Package paths resolves the directories used by OpenCode.
package paths

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Paths holds every OpenCode path the garbage collector cares about.
type Paths struct {
	Home       string
	Data       string
	Cache      string
	Config     string
	State      string
	Tmp        string
	Bin        string
	Log        string
	Repos      string
	DB         string
	Shell      string
	ToolOutput string
	BackupDir  string
}

// Resolve detects paths using `opencode debug paths` when useCLI is true and
// falls back to the documented XDG-like defaults.
func Resolve(ctx context.Context, useCLI bool) Paths {
	var p Paths
	if useCLI {
		if detected, ok := fromCLI(ctx); ok {
			p = detected
		} else {
			p = defaults()
		}
	} else {
		p = defaults()
	}
	p.fill()
	return p
}

func fromCLI(parent context.Context) (Paths, bool) {
	exe, err := exec.LookPath("opencode")
	if err != nil {
		return Paths{}, false
	}

	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "debug", "paths")
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return Paths{}, false
	}

	var p Paths
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.ToLower(fields[0])
		value := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		if value == "" {
			continue
		}
		switch key {
		case "home":
			p.Home = value
		case "data":
			p.Data = value
		case "cache":
			p.Cache = value
		case "config":
			p.Config = value
		case "state":
			p.State = value
		case "tmp":
			p.Tmp = value
		case "bin":
			p.Bin = value
		case "log":
			p.Log = value
		case "repos":
			p.Repos = value
		case "db":
			p.DB = value
		}
	}

	if p.Data == "" {
		return Paths{}, false
	}
	return p, true
}

func defaults() Paths {
	home, _ := os.UserHomeDir()
	p := Paths{Home: home}

	dataCandidates := []string{}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		dataCandidates = append(dataCandidates, filepath.Join(v, "opencode"))
	}
	dataCandidates = append(dataCandidates,
		filepath.Join(home, ".local", "share", "opencode"),
		filepath.Join(home, "Library", "Application Support", "opencode"),
	)
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		dataCandidates = append(dataCandidates, filepath.Join(v, "opencode"))
	}
	p.Data = firstExisting(dataCandidates, filepath.Join(home, ".local", "share", "opencode"))

	cacheCandidates := []string{}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		cacheCandidates = append(cacheCandidates, filepath.Join(v, "opencode"))
	}
	cacheCandidates = append(cacheCandidates,
		filepath.Join(home, ".cache", "opencode"),
		filepath.Join(home, "Library", "Caches", "opencode"),
	)
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		cacheCandidates = append(cacheCandidates, filepath.Join(v, "opencode"))
	}
	p.Cache = firstExisting(cacheCandidates, filepath.Join(home, ".cache", "opencode"))

	configCandidates := []string{}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		configCandidates = append(configCandidates, filepath.Join(v, "opencode"))
	}
	configCandidates = append(configCandidates,
		filepath.Join(home, ".config", "opencode"),
		filepath.Join(home, "Library", "Application Support", "opencode"),
	)
	if v := os.Getenv("APPDATA"); v != "" {
		configCandidates = append(configCandidates, filepath.Join(v, "opencode"))
	}
	p.Config = firstExisting(configCandidates, filepath.Join(home, ".config", "opencode"))

	stateCandidates := []string{}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		stateCandidates = append(stateCandidates, filepath.Join(v, "opencode"))
	}
	stateCandidates = append(stateCandidates, filepath.Join(home, ".local", "state", "opencode"))
	p.State = firstExisting(stateCandidates, filepath.Join(home, ".local", "state", "opencode"))

	p.Tmp = filepath.Join(os.TempDir(), "opencode")
	return p
}

func (p *Paths) fill() {
	if p.Home == "" {
		p.Home, _ = os.UserHomeDir()
	}
	if p.Data == "" {
		p.Data = filepath.Join(p.Home, ".local", "share", "opencode")
	}
	if p.Cache == "" {
		p.Cache = filepath.Join(p.Home, ".cache", "opencode")
	}
	if p.Config == "" {
		p.Config = filepath.Join(p.Home, ".config", "opencode")
	}
	if p.State == "" {
		p.State = filepath.Join(p.Home, ".local", "state", "opencode")
	}
	if p.Tmp == "" {
		p.Tmp = filepath.Join(os.TempDir(), "opencode")
	}
	if p.Bin == "" {
		p.Bin = filepath.Join(p.Cache, "bin")
	}
	if p.Log == "" {
		p.Log = filepath.Join(p.Data, "log")
	}
	if p.Repos == "" {
		p.Repos = filepath.Join(p.Data, "repos")
	}
	if p.DB == "" {
		p.DB = filepath.Join(p.Data, "opencode.db")
	}
	if p.Shell == "" {
		p.Shell = filepath.Join(p.Data, "shell")
	}
	if p.ToolOutput == "" {
		p.ToolOutput = filepath.Join(p.Data, "tool-output")
	}
	if p.BackupDir == "" {
		p.BackupDir = filepath.Join(p.Data, "backups")
	}
	p.Home = expand(p.Home)
	p.Data = abs(expand(p.Data))
	p.Cache = abs(expand(p.Cache))
	p.Config = abs(expand(p.Config))
	p.State = abs(expand(p.State))
	p.Tmp = abs(expand(p.Tmp))
	p.Bin = abs(expand(p.Bin))
	p.Log = abs(expand(p.Log))
	p.Repos = abs(expand(p.Repos))
	p.DB = abs(expand(p.DB))
	p.Shell = abs(expand(p.Shell))
	p.ToolOutput = abs(expand(p.ToolOutput))
	p.BackupDir = abs(expand(p.BackupDir))
}

// Apply applies command line overrides. Overriding the data directory also
// re-derives every path that lives inside it unless it was given explicitly.
func (p *Paths) Apply(dataDir, tmpDir, dbPath, backupDir string) {
	if dataDir != "" {
		p.Data = abs(expand(dataDir))
		p.Log = filepath.Join(p.Data, "log")
		p.Repos = filepath.Join(p.Data, "repos")
		p.Shell = filepath.Join(p.Data, "shell")
		p.ToolOutput = filepath.Join(p.Data, "tool-output")
		p.BackupDir = filepath.Join(p.Data, "backups")
		p.DB = filepath.Join(p.Data, "opencode.db")
	}
	if tmpDir != "" {
		p.Tmp = abs(expand(tmpDir))
	}
	if dbPath != "" {
		p.DB = abs(expand(dbPath))
		if dataDir == "" && backupDir == "" {
			p.BackupDir = filepath.Join(filepath.Dir(p.DB), "backups")
		}
	}
	if backupDir != "" {
		p.BackupDir = abs(expand(backupDir))
	}
}

func firstExisting(candidates []string, fallback string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return fallback
}

func abs(p string) string {
	if p == "" {
		return p
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func expand(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}
