// Package service detects whether the shared OpenCode background service is
// running, so that database maintenance can refuse to run while it is in use.
package service

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Info describes the detected service state.
type Info struct {
	Running bool
	PID     int
	URL     string
	Source  string
}

type serviceFile struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
}

// Detect checks the service state file and, as a fallback, asks the CLI.
func Detect(ctx context.Context, stateDir string) Info {
	if stateDir != "" {
		path := filepath.Join(stateDir, "service.json")
		if data, err := os.ReadFile(path); err == nil {
			var f serviceFile
			if json.Unmarshal(data, &f) == nil {
				if f.URL != "" {
					aliveNow := alive(f.URL)
					return Info{Running: aliveNow, PID: f.PID, URL: f.URL, Source: "service.json"}
				}
				if f.PID > 0 {
					return Info{Running: false, PID: f.PID, Source: "service.json"}
				}
			}
		}
	}

	exe, err := exec.LookPath("opencode")
	if err != nil {
		return Info{}
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, exe, "service", "status").CombinedOutput()
	if err != nil {
		return Info{Source: "cli"}
	}
	text := strings.TrimSpace(string(out))
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		return Info{Running: true, URL: text, Source: "cli"}
	}
	return Info{Running: false, Source: "cli"}
}

func alive(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", u.Host, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
