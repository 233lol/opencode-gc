package gc

import (
	"testing"
	"time"

	"opencode-gc/internal/dbgc"
	"opencode-gc/internal/report"
)

func TestSortSessionRows(t *testing.T) {
	rows := []report.SessionRow{
		{ID: "a", TotalBytes: 10, Messages: 1, Updated: 3, Created: 1, Cost: 0.1, Tokens: 5},
		{ID: "b", TotalBytes: 30, Messages: 2, Updated: 1, Created: 2, Cost: 0.3, Tokens: 1},
		{ID: "c", TotalBytes: 20, Messages: 3, Updated: 2, Created: 3, Cost: 0.2, Tokens: 9},
	}
	cases := []struct {
		sort string
		want string
	}{
		{"size", "b"},
		{"messages", "c"},
		{"updated", "a"},
		{"created", "c"},
		{"cost", "b"},
		{"tokens", "c"},
	}
	for _, c := range cases {
		got := append([]report.SessionRow(nil), rows...)
		if err := sortSessionRows(got, c.sort); err != nil {
			t.Fatalf("%s: %v", c.sort, err)
		}
		if got[0].ID != c.want {
			t.Fatalf("%s first = %s, want %s", c.sort, got[0].ID, c.want)
		}
	}
	if err := sortSessionRows(rows, "nope"); err == nil {
		t.Fatal("invalid sort should fail")
	}
}

func TestFilterSessionRows(t *testing.T) {
	now := time.Now()
	rows := []report.SessionRow{
		{ID: "ses_a", Project: "elona", Worktree: "C:/elona", Updated: now.Add(-40 * 24 * time.Hour).UnixMilli()},
		{ID: "ses_b", Project: "other", Worktree: "C:/other", Updated: now.Add(-1 * time.Hour).UnixMilli()},
	}
	got := filterSessionRows(rows, "elona", 0, now)
	if len(got) != 1 || got[0].ID != "ses_a" {
		t.Fatalf("project filter = %#v", got)
	}
	got = filterSessionRows(rows, "", 7*24*time.Hour, now)
	if len(got) != 1 || got[0].ID != "ses_a" {
		t.Fatalf("older filter = %#v", got)
	}
}

func TestDescendants(t *testing.T) {
	meta := &dbgc.Meta{Children: map[string][]string{
		"a": {"b", "c"},
		"b": {"d"},
	}}
	got := descendants(meta, "a")
	want := map[string]bool{"b": true, "c": true, "d": true}
	if len(got) != len(want) {
		t.Fatalf("descendants = %#v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected descendant %s", id)
		}
	}
}
