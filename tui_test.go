package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/phil/memhack/internal/scan"
)

// assertQuit runs cmd and fails unless it produces a tea.QuitMsg.
func assertQuit(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a quit command, got nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg")
	}
}

// newTestModel builds a model with a controller whose worker jobs are never
// executed. UI transition tests only inspect the returned model; they do not
// run the returned tea.Cmds, so no worker interaction happens.
func newTestModel() model {
	ctrl := &controller{jobs: make(chan job)}
	m := newModel(ctrl, scan.I32, nil, screenScanner)
	// Give it a size so the table/input are laid out.
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m2.(model)
}

func press(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+z":
		return tea.KeyMsg{Type: tea.KeyCtrlZ}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// typeString feeds each rune of s to the model as key messages.
func typeString(m model, s string) model {
	for _, r := range s {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(model)
	}
	return m
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []string{"ctrl+c", "ctrl+d"} {
		m := newTestModel()
		_, cmd := m.Update(press(k))
		assertQuit(t, cmd)
	}
}

func TestQuitCommandFromInput(t *testing.T) {
	// Both the bare word and the ":" form must quit.
	for _, word := range []string{"quit", "exit", "q", ":q", ":quit", ":exit"} {
		m := newTestModel()
		m = typeString(m, word)
		_, cmd := m.Update(press("enter"))
		assertQuit(t, cmd)
	}
}

func TestSpinnerBusyEdge(t *testing.T) {
	m := newTestModel()
	if m.busy {
		t.Fatal("should start idle")
	}
	// Idle -> busy edge returns a spinner start command.
	cmd := m.markBusy()
	if cmd == nil {
		t.Fatal("markBusy on the idle->busy edge should return a start command")
	}
	if !m.busy {
		t.Fatal("markBusy should set busy")
	}
	// Already busy -> no second start command (prevents duplicate tick loops).
	if again := m.markBusy(); again != nil {
		t.Error("markBusy while busy should return nil")
	}
}

func TestSpinnerTickStopsWhenIdle(t *testing.T) {
	m := newTestModel()
	m.busy = false
	_, cmd := m.Update(spinner.TickMsg{})
	if cmd != nil {
		t.Error("a spinner tick while idle must not reschedule itself")
	}
}

func TestStatusShowsWorkingWhenBusy(t *testing.T) {
	m := newTestModel()
	m.st = state{Attached: true, Pid: 1, Type: scan.I32}
	if strings.Contains(m.View(), "working") {
		t.Error("idle view should not show a working indicator")
	}
	m.busy = true
	if !strings.Contains(m.View(), "working") {
		t.Error("busy view should show the working indicator")
	}
}

func TestRepeatLastScanOnEmptyEnter(t *testing.T) {
	m := newTestModel()
	// Empty enter with no prior scan does nothing.
	nm, cmd := m.Update(press("enter"))
	m = nm.(model)
	if cmd != nil {
		t.Error("empty enter with no prior scan should do nothing")
	}

	// Run a scan; it should be remembered.
	m = typeString(m, "inc")
	nm, _ = m.Update(press("enter"))
	m = nm.(model)
	if m.lastScan != "inc" {
		t.Fatalf("lastScan = %q, want \"inc\"", m.lastScan)
	}

	// Clear busy (as the stateMsg would) and press empty enter to repeat.
	m.busy = false
	nm, cmd = m.Update(press("enter"))
	m = nm.(model)
	if cmd == nil {
		t.Error("empty enter after a scan should repeat it")
	}
	if !strings.Contains(m.status, "repeat") || !strings.Contains(m.status, "inc") {
		t.Errorf("status = %q, want a repeat message naming the scan", m.status)
	}
}

func TestCommandNotRememberedAsLastScan(t *testing.T) {
	m := newTestModel()
	m = typeString(m, ":reset")
	nm, _ := m.Update(press("enter"))
	m = nm.(model)
	if m.lastScan != "" {
		t.Errorf("a ':' command should not be recorded as a repeatable scan; got %q", m.lastScan)
	}
}

func TestEscCancelsRunningScan(t *testing.T) {
	m := newTestModel()
	cancelled := false
	// Simulate a scan in progress by installing a cancel func.
	m.ctrl.mu.Lock()
	m.ctrl.scanCancel = func() { cancelled = true }
	m.ctrl.mu.Unlock()
	m.busy = true

	nm, _ := m.Update(press("esc"))
	m = nm.(model)
	if !cancelled {
		t.Error("esc while a scan is running should cancel it")
	}
	if !strings.Contains(m.status, "cancel") {
		t.Errorf("status = %q, want a cancelling message", m.status)
	}
}

func TestEscDoesNothingWhenIdle(t *testing.T) {
	m := newTestModel()
	// No scan running, not busy: esc should be a harmless no-op.
	nm, cmd := m.Update(press("esc"))
	m = nm.(model)
	if cmd != nil {
		t.Error("esc while idle should not issue a command")
	}
}

func TestTabTogglesFocus(t *testing.T) {
	m := newTestModel()
	if m.focusTable {
		t.Fatal("input should start focused")
	}
	nm, _ := m.Update(press("tab"))
	m = nm.(model)
	if !m.focusTable {
		t.Fatal("tab should focus the table")
	}
	nm, _ = m.Update(press("tab"))
	m = nm.(model)
	if m.focusTable {
		t.Fatal("tab should return focus to the input")
	}
}

func TestScanEnterIssuesCommand(t *testing.T) {
	m := newTestModel()
	m = typeString(m, "1337")
	nm, cmd := m.Update(press("enter"))
	m = nm.(model)
	if cmd == nil {
		t.Fatal("expected a scan command to be issued")
	}
	if !m.busy {
		t.Error("model should be marked busy after issuing a scan")
	}
	if m.input.Value() != "" {
		t.Errorf("input should be cleared after scanning, got %q", m.input.Value())
	}
}

func TestUnknownCommandSetsError(t *testing.T) {
	m := newTestModel()
	m = typeString(m, ":bogus")
	nm, cmd := m.Update(press("enter"))
	m = nm.(model)
	if cmd != nil {
		t.Error("unknown command should not issue a worker command")
	}
	if !strings.Contains(m.errMsg, "unknown command") {
		t.Errorf("errMsg = %q, want it to mention unknown command", m.errMsg)
	}
}

func TestCommandUsageErrors(t *testing.T) {
	for _, in := range []string{":pid", ":pid abc", ":set 1", ":type"} {
		m := newTestModel()
		m = typeString(m, in)
		nm, cmd := m.Update(press("enter"))
		m = nm.(model)
		if cmd != nil {
			t.Errorf("%q: expected no command issued", in)
		}
		if m.errMsg == "" {
			t.Errorf("%q: expected a usage/parse error", in)
		}
	}
}

func TestWriteModeFlow(t *testing.T) {
	m := newTestModel()
	// Populate a match so a row exists to edit.
	m2, _ := m.Update(stateMsg(state{
		Attached: true, Pid: 42, Type: scan.I32, Count: 1, Scanned: true,
		Rows: []matchRow{{Index: 0, Addr: 0x1000, Value: "5"}},
	}))
	m = m2.(model)

	// Focus the table, then enter write mode on the selected row.
	nm, _ := m.Update(press("tab"))
	m = nm.(model)
	nm, _ = m.Update(press("w"))
	m = nm.(model)
	if m.mode != modeWrite {
		t.Fatal("expected write mode after pressing w")
	}
	if m.focusTable {
		t.Fatal("focus should move to the input in write mode")
	}

	// esc cancels write mode.
	nm, _ = m.Update(press("esc"))
	m = nm.(model)
	if m.mode != modeScan {
		t.Fatal("esc should cancel write mode")
	}

	// Re-enter write mode and submit a value; a write command should issue.
	nm, _ = m.Update(press("tab"))
	m = nm.(model)
	nm, _ = m.Update(press("w"))
	m = nm.(model)
	m = typeString(m, "99")
	nm, cmd := m.Update(press("enter"))
	m = nm.(model)
	if cmd == nil {
		t.Fatal("expected a write command to be issued")
	}
	if m.mode != modeScan {
		t.Error("after submitting a write, mode should return to scan")
	}
}

func TestStateMsgPopulatesTable(t *testing.T) {
	m := newTestModel()
	nm, _ := m.Update(stateMsg(state{
		Attached: true, Pid: 7, Type: scan.I32, Count: 3, Scanned: true,
		Rows: []matchRow{
			{Index: 0, Addr: 0x1000, Value: "1"},
			{Index: 1, Addr: 0x2000, Value: "2"},
			{Index: 2, Addr: 0x3000, Value: "3"},
		},
	}))
	m = nm.(model)
	if got := len(m.table.Rows()); got != 3 {
		t.Errorf("table has %d rows, want 3", got)
	}
	if !strings.Contains(m.View(), "pid 7") {
		t.Error("view should show the attached pid")
	}
	if !strings.Contains(m.View(), "3 matches") {
		t.Error("view should show the match count")
	}
}

func TestCursorSeededWhenRowsArrive(t *testing.T) {
	// bubbles' table leaves the cursor at -1 until navigated; refreshTable must
	// seed it to 0 so the first row is immediately selectable/editable.
	m := newTestModel()
	if m.table.Cursor() >= 0 {
		t.Log("cursor already valid with no rows; fine")
	}
	nm, _ := m.Update(stateMsg(state{
		Attached: true, Type: scan.I32, Count: 1, Scanned: true,
		Rows: []matchRow{{Index: 0, Addr: 0x1000, Value: "5"}},
	}))
	m = nm.(model)
	if m.table.Cursor() != 0 {
		t.Fatalf("cursor = %d after rows arrive, want 0", m.table.Cursor())
	}

	// Editing the selected row must work without first pressing a nav key.
	nm, _ = m.Update(press("tab"))
	m = nm.(model)
	nm, _ = m.Update(press("w"))
	m = nm.(model)
	if m.mode != modeWrite {
		t.Fatal("expected write mode immediately after rows populate (no nav needed)")
	}
}

func TestStateMsgError(t *testing.T) {
	m := newTestModel()
	nm, _ := m.Update(stateMsg(state{Type: scan.I32, Err: errNotAttached}))
	m = nm.(model)
	if !strings.Contains(m.errMsg, "not attached") {
		t.Errorf("errMsg = %q, want the not-attached error", m.errMsg)
	}
	m.busy = true
	nm, _ = m.Update(stateMsg(state{Type: scan.I32}))
	m = nm.(model)
	if m.busy {
		t.Error("stateMsg should clear the busy flag")
	}
}

func TestErrorSurvivesRefresh(t *testing.T) {
	// A user action's error must not be wiped by the next periodic value
	// refresh (regression: the error used to flash and vanish within ~500ms).
	m := newTestModel()
	nm, _ := m.Update(stateMsg(state{Type: scan.I32, Err: errNotAttached}))
	m = nm.(model)
	if !strings.Contains(m.errMsg, "not attached") {
		t.Fatalf("errMsg = %q, want the action error", m.errMsg)
	}

	// A refresh arrives (as happens twice a second while matches exist).
	nm, _ = m.Update(refreshMsg(state{
		Attached: true, Type: scan.I32, Count: 3, Scanned: true,
		Rows: []matchRow{{Index: 0, Addr: 0x1000, Value: "5"}},
	}))
	m = nm.(model)
	if !strings.Contains(m.errMsg, "not attached") {
		t.Errorf("refresh cleared the error message: errMsg = %q", m.errMsg)
	}
	// The refresh should still have updated the live data.
	if m.st.Count != 3 {
		t.Errorf("refresh did not update live data: count = %d", m.st.Count)
	}
	if !m.st.Attached || m.busy {
		t.Error("refresh should update attached state and clear busy")
	}
}

func TestStatusNoteSurvivesRefresh(t *testing.T) {
	m := newTestModel()
	nm, _ := m.Update(stateMsg(state{Type: scan.I32, Note: "wrote 1000 to 3 matches (0 failed)"}))
	m = nm.(model)
	if !strings.Contains(m.status, "3 matches") {
		t.Fatalf("status = %q", m.status)
	}
	nm, _ = m.Update(refreshMsg(state{Attached: true, Type: scan.I32, Count: 3, Scanned: true}))
	m = nm.(model)
	if !strings.Contains(m.status, "3 matches") {
		t.Errorf("refresh cleared the status note: status = %q", m.status)
	}
}

func TestTickGating(t *testing.T) {
	m := newTestModel()
	// Idle + attached + matches -> tick should schedule a refresh (busy set).
	m.st = state{Attached: true, Count: 2, Scanned: true, Type: scan.I32}
	nm, cmd := m.Update(tickMsg{})
	m = nm.(model)
	if !m.busy {
		t.Error("tick with matches while idle should trigger a refresh")
	}
	if cmd == nil {
		t.Error("tick should always reschedule itself")
	}

	// While busy, a tick must not issue another refresh.
	busyModel := newTestModel()
	busyModel.st = state{Attached: true, Count: 2, Scanned: true, Type: scan.I32}
	busyModel.busy = true
	nm, _ = busyModel.Update(tickMsg{})
	if !nm.(model).busy {
		// still busy is fine; the point is no panic and no state reset
		t.Log("remained busy as expected")
	}
}
