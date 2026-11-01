package gc

import (
	"testing"

	"opencode-gc/internal/dbgc"
)

func TestPlanHasChanges(t *testing.T) {
	empty := &dbgc.Plan{Orphans: []dbgc.OrphanRule{{Name: "x", Count: 0}}}
	if planHasChanges(empty, Selection{DBOrphans: true}) {
		t.Fatal("zero-count orphans must not trigger a write")
	}

	orphans := &dbgc.Plan{Orphans: []dbgc.OrphanRule{{Name: "x", Count: 1}}}
	if !planHasChanges(orphans, Selection{DBOrphans: true}) {
		t.Fatal("non-zero orphans should trigger a write")
	}
	if planHasChanges(orphans, Selection{DBOrphans: false}) {
		t.Fatal("unselected orphans must not trigger a write")
	}

	if !planHasChanges(&dbgc.Plan{Sessions: []dbgc.SessionCandidate{{}}}, Selection{Sessions: true}) {
		t.Fatal("sessions should trigger a write")
	}
	if !planHasChanges(&dbgc.Plan{Projects: []dbgc.ProjectCandidate{{}}}, Selection{DBProjects: true}) {
		t.Fatal("projects should trigger a write")
	}
	if !planHasChanges(&dbgc.Plan{Archived: []dbgc.SessionCandidate{{}}}, Selection{Archived: true}) {
		t.Fatal("archived sessions should trigger a write")
	}
	if !planHasChanges(&dbgc.Plan{ReasoningCount: 1}, Selection{Reasoning: true}) {
		t.Fatal("reasoning should trigger a write")
	}
	if !planHasChanges(&dbgc.Plan{DBCacheKeys: []string{"k"}}, Selection{DBCache: true}) {
		t.Fatal("cache keys should trigger a write")
	}
}

func TestSelection(t *testing.T) {
	safe := DefaultSelection()
	if !safe.Tmp || !safe.Logs || !safe.Shell || !safe.DBOrphans {
		t.Fatalf("unexpected default selection: %#v", safe)
	}
	if safe.ToolOutput || safe.Sessions || safe.Reasoning || safe.Vacuum {
		t.Fatalf("default selection is too broad: %#v", safe)
	}
	all := AllSelection()
	if !all.ToolOutput || !all.Vacuum || !all.Repos || !all.DBCache || !all.DBProjects {
		t.Fatalf("unexpected --all selection: %#v", all)
	}
	if all.Sessions || all.Archived || all.Reasoning {
		t.Fatalf("--all must not include history: %#v", all)
	}
	if !safe.ForCategory("tmp") || safe.ForCategory("db-sessions") {
		t.Fatal("category mapping is wrong")
	}
}
