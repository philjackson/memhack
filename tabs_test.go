package main

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/phil/memhack/internal/scan"
)

func TestTabSetOpenSelectCloseCycle(t *testing.T) {
	s := newTabSet(scan.I32, 0)
	if len(s.tabs) != 1 || s.cur != 0 {
		t.Fatalf("a new set should hold exactly one tab, got %d (cur %d)", len(s.tabs), s.cur)
	}

	// Opening switches to the new tab.
	if _, err := s.open("health"); err != nil {
		t.Fatalf("open: %v", err)
	}
	if s.cur != 1 || s.active().name != "health" {
		t.Fatalf("open should switch to the new tab; cur = %d, name = %q", s.cur, s.active().name)
	}

	if err := s.selectAt(0); err != nil {
		t.Fatalf("selectAt(0): %v", err)
	}
	if err := s.selectAt(2); err == nil {
		t.Error("selecting a tab that isn't open should fail")
	}
	if err := s.selectAt(-1); err == nil {
		t.Error("selecting a negative tab should fail")
	}

	// Cycling wraps at both ends.
	s.cycle(+1)
	if s.cur != 1 {
		t.Errorf("cycle(+1) from 0 = %d, want 1", s.cur)
	}
	s.cycle(+1)
	if s.cur != 0 {
		t.Errorf("cycle past the last tab should wrap to 0, got %d", s.cur)
	}
	s.cycle(-1)
	if s.cur != 1 {
		t.Errorf("cycle(-1) from 0 should wrap to the last tab, got %d", s.cur)
	}

	closed, err := s.closeActive()
	if err != nil {
		t.Fatalf("closeActive: %v", err)
	}
	if closed.name != "health" || len(s.tabs) != 1 || s.cur != 0 {
		t.Errorf("after closing the last-listed tab: closed %q, %d left, cur %d", closed.name, len(s.tabs), s.cur)
	}
}

func TestTabSetLimit(t *testing.T) {
	s := newTabSet(scan.I32, 0)
	for i := 1; i < maxTabs; i++ {
		if _, err := s.open(""); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	if _, err := s.open(""); err == nil {
		t.Errorf("opening past %d tabs should be refused", maxTabs)
	}
	if len(s.tabs) != maxTabs {
		t.Errorf("%d tabs open, want %d", len(s.tabs), maxTabs)
	}
}

func TestTabSetKeepsTheLastTab(t *testing.T) {
	// There is always somewhere to scan: the final tab is reset, not closed.
	s := newTabSet(scan.I32, 0)
	if _, err := s.closeActive(); err == nil {
		t.Error("closing the only open tab should be refused")
	}
	if len(s.tabs) != 1 {
		t.Errorf("the refused close should leave the tab open, got %d", len(s.tabs))
	}
}

func TestTabLabelAndSummary(t *testing.T) {
	s := newTabSet(scan.I32, 0)
	if got := s.active().label(); got != "empty" {
		t.Errorf("an untouched tab should be labelled %q, got %q", "empty", got)
	}
	// A scan names an unnamed tab...
	s.active().lastScan = "> 100"
	if got := s.active().label(); got != "> 100" {
		t.Errorf("label = %q, want the last scan", got)
	}
	// ...and an explicit name wins over it.
	s.rename("  health  ")
	if got := s.active().label(); got != "health" {
		t.Errorf("label = %q, want the trimmed name", got)
	}
	s.rename("")
	if got := s.active().label(); got != "> 100" {
		t.Errorf("clearing the name should fall back to the last scan, got %q", got)
	}

	if _, err := s.open("ammo"); err != nil {
		t.Fatalf("open: %v", err)
	}
	sum := s.summary()
	if !strings.Contains(sum, "> 100") || !strings.Contains(sum, "ammo") {
		t.Errorf("summary should name every tab: %q", sum)
	}
	if !strings.Contains(sum, "▸2") {
		t.Errorf("summary should mark the current tab: %q", sum)
	}
}

func TestParseTabCmd(t *testing.T) {
	tests := []struct {
		in     []string
		action tabAction
		name   string
		index  int
	}{
		{in: nil, action: tabList},
		{in: []string{"new"}, action: tabNew},
		{in: []string{"new", "low", "health"}, action: tabNew, name: "low health"},
		{in: []string{"close"}, action: tabClose},
		{in: []string{"rename", "ammo"}, action: tabRename, name: "ammo"},
		{in: []string{"3"}, action: tabSelect, index: 2},
	}
	for _, tt := range tests {
		got, err := parseTabCmd(tt.in)
		if err != nil {
			t.Errorf("parseTabCmd(%v): %v", tt.in, err)
			continue
		}
		if got.action != tt.action || got.name != tt.name || got.index != tt.index {
			t.Errorf("parseTabCmd(%v) = %+v, want action %v name %q index %d", tt.in, got, tt.action, tt.name, tt.index)
		}
	}
	for _, in := range [][]string{{"rename"}, {"bogus"}, {"1.5"}} {
		if _, err := parseTabCmd(in); err == nil {
			t.Errorf("parseTabCmd(%v): expected an error", in)
		}
	}
}

func TestWorkerTabSettingsAreIndependent(t *testing.T) {
	// No process needed: types and alignment are tab state, set before or after
	// attaching.
	w := newWorker(scan.I32, time.Hour, 0)
	if _, err := w.setAlign(1); err != nil {
		t.Fatalf("setAlign: %v", err)
	}
	if _, err := w.newTab("floats"); err != nil {
		t.Fatalf("newTab: %v", err)
	}
	// A new tab inherits the current tab's type and alignment.
	if got := w.active().align; got != 1 {
		t.Errorf("new tab alignment = %d, want the inherited 1", got)
	}
	if _, err := w.setType("f32"); err != nil {
		t.Fatalf("setType: %v", err)
	}
	if _, err := w.setAlign(0); err != nil {
		t.Fatalf("setAlign: %v", err)
	}

	// Changing this tab's type/alignment leaves the other tab's alone.
	if _, err := w.selectTab(0); err != nil {
		t.Fatalf("selectTab: %v", err)
	}
	if got := w.active().dt; got != scan.I32 {
		t.Errorf("tab 1 type = %s, want i32", got)
	}
	if got := w.active().align; got != 1 {
		t.Errorf("tab 1 alignment = %d, want 1", got)
	}
	if got := w.tabs[1].dt; got != scan.F32 {
		t.Errorf("tab 2 type = %s, want f32", got)
	}
}

func TestWorkerSnapshotDescribesEveryTab(t *testing.T) {
	w := newWorker(scan.I32, time.Hour, 0)
	if _, err := w.newTab("ammo"); err != nil {
		t.Fatalf("newTab: %v", err)
	}
	st := w.snapshot()
	if len(st.Tabs) != 2 {
		t.Fatalf("snapshot has %d tabs, want 2", len(st.Tabs))
	}
	if st.Active != 1 {
		t.Errorf("Active = %d, want 1 (the newly opened tab)", st.Active)
	}
	if st.Tabs[1].Label != "ammo" {
		t.Errorf("tab 2 label = %q, want ammo", st.Tabs[1].Label)
	}
	if st.Tabs[0].Scanned {
		t.Error("an unscanned tab should not be reported as scanned")
	}
}

func TestWorkerCycleAndCloseNotes(t *testing.T) {
	w := newWorker(scan.I32, time.Hour, 0)
	// With one tab open, cycling says so rather than pretending to switch.
	note, err := w.cycleTab(+1)
	if err != nil || !strings.Contains(note, "one tab") {
		t.Errorf("cycleTab with a single tab: note = %q, err = %v", note, err)
	}
	if _, err := w.newTab("ammo"); err != nil {
		t.Fatalf("newTab: %v", err)
	}
	if note, err = w.cycleTab(+1); err != nil || !strings.Contains(note, "tab 1 of 2") {
		t.Errorf("cycleTab: note = %q, err = %v", note, err)
	}
	if _, err := w.closeTab(); err != nil {
		t.Fatalf("closeTab: %v", err)
	}
	if len(w.tabs) != 1 || w.active().name != "ammo" {
		t.Errorf("after closing tab 1, %d tabs left on %q", len(w.tabs), w.active().name)
	}
	if _, err := w.closeTab(); err == nil {
		t.Error("closing the last tab should be refused")
	}
}

// TestTabsHoldSeparateSearches drives a real target: two tabs must narrow
// independently, freezes must hold across a tab switch, and re-attaching must
// clear every tab.
func TestTabsHoldSeparateSearches(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	bin := buildCTarget(t)
	w := newWorker(scan.I32, time.Hour, 0)
	if _, err := w.launch([]string{bin}); err != nil {
		if strings.Contains(err.Error(), "not permitted") || strings.Contains(err.Error(), "denied") {
			t.Skipf("ptrace not permitted here: %v", err)
		}
		t.Fatalf("launch: %v", err)
	}
	pid := w.proc.Pid
	defer func() {
		w.detach()
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	}()

	if _, err := w.scanExpr(context.Background(), "1337"); err != nil {
		t.Fatalf("scan in tab 1: %v", err)
	}
	firstCount := len(w.active().sc.Matches)
	if firstCount == 0 {
		t.Fatal("expected matches for 1337 in tab 1")
	}

	// A second tab is attached but starts unscanned.
	if _, err := w.newTab("other"); err != nil {
		t.Fatalf("newTab: %v", err)
	}
	if w.active().sc == nil {
		t.Fatal("a tab opened while attached should have a scanner")
	}
	if w.active().sc.Scanned() || len(w.active().sc.Matches) != 0 {
		t.Fatal("a new tab should start with no matches")
	}

	// Scanning here must not disturb tab 1's match set.
	if _, err := w.scanExpr(context.Background(), "0"); err != nil {
		t.Fatalf("scan in tab 2: %v", err)
	}
	if got := len(w.tabs[0].sc.Matches); got != firstCount {
		t.Errorf("tab 1 has %d matches after scanning tab 2, want %d", got, firstCount)
	}

	// Freeze a match in tab 1, then switch away: the freeze belongs to the
	// target, so it must go on being applied.
	if _, err := w.selectTab(0); err != nil {
		t.Fatalf("selectTab: %v", err)
	}
	if _, err := w.freezeIndex(0); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, err := w.write(0, "9999"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.selectTab(1); err != nil {
		t.Fatalf("selectTab: %v", err)
	}
	if st := w.snapshot(); st.Frozen != 1 {
		t.Errorf("Frozen = %d in the other tab, want the freeze to be shared", st.Frozen)
	}
	w.applyFreezes()
	w.tabs[0].sc.RefreshN(1)
	if got := scan.I32.Format(w.tabs[0].sc.Matches[0].Last); got != "1337" {
		t.Errorf("freeze stopped holding after a tab switch: value = %s, want 1337", got)
	}

	// Re-attaching invalidates every tab's addresses, so all of them reset.
	if _, err := w.attach(pid); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	for i, tb := range w.tabs {
		if tb.sc == nil {
			t.Fatalf("tab %d has no scanner after re-attaching", i+1)
		}
		if tb.sc.Scanned() || len(tb.sc.Matches) != 0 {
			t.Errorf("tab %d kept %d matches across a re-attach", i+1, len(tb.sc.Matches))
		}
	}
	if len(w.frozen) != 0 {
		t.Errorf("re-attaching should drop the old process's freezes, got %d", len(w.frozen))
	}
}
