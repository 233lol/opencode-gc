package main

import (
	"testing"
	"time"

	"opencode-gc/internal/age"
	"opencode-gc/internal/gc"
)

func dur(d time.Duration) age.Duration { return age.Duration{Duration: d} }

func TestResolveAgesDefaults(t *testing.T) {
	a := resolveAges(fileOpts{}, dbOpts{})
	if a.file != 7*24*time.Hour {
		t.Fatalf("file = %v", a.file)
	}
	if a.project != 30*24*time.Hour {
		t.Fatalf("project = %v", a.project)
	}
	if a.session != 0 || a.archived != 0 || a.reasoning != 0 {
		t.Fatalf("history should be disabled: %#v", a)
	}
}

func TestResolveAgesOlderThan(t *testing.T) {
	older := dur(10 * 24 * time.Hour)
	a := resolveAges(fileOpts{OlderThan: older}, dbOpts{})
	if a.file != older.Duration || a.project != older.Duration || a.session != older.Duration ||
		a.archived != older.Duration || a.reasoning != older.Duration {
		t.Fatalf("--older-than was not applied: %#v", a)
	}
}

func TestResolveAgesOverrides(t *testing.T) {
	a := resolveAges(
		fileOpts{OlderThan: dur(30 * 24 * time.Hour), Age: dur(7 * 24 * time.Hour)},
		dbOpts{ProjectAge: dur(60 * 24 * time.Hour), Sessions: dur(90 * 24 * time.Hour)},
	)
	if a.file != 7*24*time.Hour {
		t.Fatalf("file = %v", a.file)
	}
	if a.project != 60*24*time.Hour {
		t.Fatalf("project = %v", a.project)
	}
	if a.session != 90*24*time.Hour {
		t.Fatalf("session = %v", a.session)
	}
	if a.archived != 30*24*time.Hour || a.reasoning != 30*24*time.Hour {
		t.Fatalf("archived/reasoning should follow --older-than: %#v", a)
	}
}

func TestCleanSelectionOlderThan(t *testing.T) {
	sel := cleanSelection(&cleanCmd{fileOpts: fileOpts{OlderThan: dur(30 * 24 * time.Hour)}})
	want := gc.UnifiedSelection()
	if sel != want {
		t.Fatalf("selection = %#v, want %#v", sel, want)
	}

	narrow := cleanSelection(&cleanCmd{
		fileOpts: fileOpts{OlderThan: dur(30 * 24 * time.Hour)},
		Tmp:      true,
	})
	if !narrow.Tmp || narrow.Sessions || narrow.DBProjects || narrow.Vacuum || narrow.Logs {
		t.Fatalf("explicit flags should narrow the selection: %#v", narrow)
	}

	all := cleanSelection(&cleanCmd{
		fileOpts: fileOpts{OlderThan: dur(30 * 24 * time.Hour)},
		All:      true,
	})
	wantAll := gc.AllSelection()
	wantAll.Sessions = true
	wantAll.Archived = true
	wantAll.Reasoning = true
	if all != wantAll {
		t.Fatalf("--all + --older-than should be additive: %#v, want %#v", all, wantAll)
	}

	allOnly := cleanSelection(&cleanCmd{All: true})
	if allOnly != gc.AllSelection() {
		t.Fatalf("--all alone = %#v", allOnly)
	}
}

func TestScanSelectionOlderThan(t *testing.T) {
	sel := scanSelection(fileOpts{OlderThan: dur(30 * 24 * time.Hour)}, dbOpts{})
	if sel != gc.UnifiedSelection() {
		t.Fatalf("selection = %#v", sel)
	}
	plain := scanSelection(fileOpts{}, dbOpts{})
	if plain != gc.DefaultSelection() {
		t.Fatalf("plain selection = %#v", plain)
	}
}

func TestLimitAgesToSelection(t *testing.T) {
	opts := gc.Options{
		SessionAge: 24 * time.Hour, ArchivedAge: 24 * time.Hour, ReasoningAge: 24 * time.Hour,
		Selection: gc.Selection{Tmp: true},
	}
	limitAgesToSelection(&opts)
	if opts.SessionAge != 0 || opts.ArchivedAge != 0 || opts.ReasoningAge != 0 {
		t.Fatalf("ages should be cleared: %#v", opts)
	}

	opts = gc.Options{
		SessionAge: 24 * time.Hour, ArchivedAge: 24 * time.Hour, ReasoningAge: 24 * time.Hour,
		Selection: gc.UnifiedSelection(),
	}
	limitAgesToSelection(&opts)
	if opts.SessionAge == 0 || opts.ArchivedAge == 0 || opts.ReasoningAge == 0 {
		t.Fatalf("ages should be kept: %#v", opts)
	}
}
