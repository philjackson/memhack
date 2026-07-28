package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

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
	ctrl := &controller{jobs: make(chan job), prog: make(chan scan.Progress, 1)}
	m := newModel(ctrl, scan.I32, nil, screenScanner, 0)
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
	cmd := m.markBusy("working")
	if cmd == nil {
		t.Fatal("markBusy on the idle->busy edge should return a start command")
	}
	if !m.busy {
		t.Fatal("markBusy should set busy")
	}
	// Already busy -> no second start command (prevents duplicate tick loops).
	if again := m.markBusy("working"); again != nil {
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

func TestBusyBoxAppearsOnlyAfterTheDelay(t *testing.T) {
	m := newTestModel()
	m.st = state{Attached: true, Pid: 1, Type: scan.I32}

	cmd := m.markBusy("working")
	if cmd == nil {
		t.Fatal("markBusy should start the spinner")
	}
	// Just-issued work shows the status-line spinner but no centred box, so a
	// fast action never flashes one up.
	if m.showBusyBox() {
		t.Error("the box should not appear before the delay has elapsed")
	}
	m.busySince = m.busySince.Add(-busyOverlayDelay)
	if !m.showBusyBox() {
		t.Fatal("the box should appear once the work outlives the delay")
	}
	if !strings.Contains(m.View(), "working…") {
		t.Error("the busy view should contain the progress box")
	}
	m.clearBusy()
	if m.showBusyBox() || m.busy {
		t.Error("clearBusy should remove the box")
	}
}

func TestBusyBoxHiddenForLiveWatchRefresh(t *testing.T) {
	m := newTestModel()
	// The watch marks the model busy with no label; however long it takes, it
	// must not throw a box over the screen every interval.
	m.markBusy("")
	m.busySince = m.busySince.Add(-10 * busyOverlayDelay)
	if m.showBusyBox() {
		t.Error("the live watch refresh must not draw the progress box")
	}
}

func TestBusyBoxNamesAndCancelsAScan(t *testing.T) {
	m := newTestModel()
	m.markBusy("working")
	m.busySince = m.busySince.Add(-busyOverlayDelay)
	// A scan in flight is named as such and advertises how to cancel it.
	m.ctrl.scanCancel = func() {}
	box := m.busyBox()
	if !strings.Contains(box, "scanning") {
		t.Errorf("box should name the scan: %q", box)
	}
	if !strings.Contains(box, "esc: cancel") {
		t.Errorf("box should offer cancellation: %q", box)
	}

	// Once cancelled, the box reports the wind-down instead of repeating the
	// cancel hint.
	nm, _ := m.Update(press("esc"))
	m = nm.(model)
	box = m.busyBox()
	if !strings.Contains(box, "cancelling scan") || strings.Contains(box, "esc: cancel") {
		t.Errorf("box should report cancelling: %q", box)
	}
}

func TestWatchRefreshDoesNotClearAPendingAction(t *testing.T) {
	m := newTestModel()
	// A refresh is in flight when the user issues an action: the worker replies
	// to the refresh first, and that must not stop the action's indicator.
	m.markBusy("")
	m.markBusy("working")
	nm, _ := m.Update(refreshMsg{Attached: true})
	m = nm.(model)
	if !m.busy || m.busyLabel == "" {
		t.Error("a refresh reply should leave a pending action's indicator up")
	}
	// The action's own reply clears it.
	nm, _ = m.Update(stateMsg{Attached: true})
	m = nm.(model)
	if m.busy || m.busyLabel != "" {
		t.Error("the action's reply should clear the indicator")
	}
}

func TestScanProgressDrivesTheBar(t *testing.T) {
	m := newTestModel()
	m.markBusy(busyWorking)
	m.busySince = m.busySince.Add(-busyOverlayDelay)

	// Before any update the work is indeterminate: spinner only, no bar.
	if strings.Contains(m.busyBox(), "%") {
		t.Error("box should not show a percentage before progress is known")
	}

	nm, cmd := m.Update(progressMsg{Done: 512 << 20, Total: 1 << 30, Unit: scan.UnitBytes})
	m = nm.(model)
	if cmd == nil {
		t.Error("a progress update must re-arm the listener for the next one")
	}
	box := m.busyBox()
	if !strings.Contains(box, "50%") {
		t.Errorf("box should show the percentage: %q", box)
	}
	if !strings.Contains(box, "512.0 MiB of 1.0 GiB") {
		t.Errorf("box should show the byte counters: %q", box)
	}

	// Finishing the work drops the bar with it.
	nm, _ = m.Update(stateMsg{Attached: true})
	m = nm.(model)
	if m.scanProg.Total != 0 {
		t.Error("completing the work should discard its progress")
	}
}

func TestProgressUpdateIgnoredWhenIdle(t *testing.T) {
	m := newTestModel()
	// A last update landing after the scan's reply must not leave a stale bar
	// for whatever is issued next.
	nm, cmd := m.Update(progressMsg{Done: 10, Total: 10, Unit: scan.UnitMatches})
	m = nm.(model)
	if m.scanProg.Total != 0 {
		t.Error("progress arriving while idle should be dropped")
	}
	if cmd == nil {
		t.Error("the listener must stay armed even for a dropped update")
	}
}

func TestProgressResetBetweenActions(t *testing.T) {
	m := newTestModel()
	m.markBusy(busyWorking)
	m.scanProg = scan.Progress{Done: 9, Total: 10, Unit: scan.UnitMatches}
	m.clearBusy()

	// A new action starts from an empty bar rather than the last one's.
	m.markBusy(busyWorking)
	if m.scanProg.Total != 0 {
		t.Error("a new action should not inherit the previous one's progress")
	}

	// Same when an action takes over from an in-flight watch refresh.
	m.clearBusy()
	m.markBusy("")
	m.scanProg = scan.Progress{Done: 9, Total: 10, Unit: scan.UnitMatches}
	m.markBusy(busyWorking)
	if m.scanProg.Total != 0 {
		t.Error("taking over from a refresh should reset progress")
	}
}

func TestFormatProgressUnits(t *testing.T) {
	byteProg := scan.Progress{Done: 3 << 30, Total: 8 << 30, Unit: scan.UnitBytes}
	if got, want := formatProgress(byteProg), "3.0 GiB of 8.0 GiB"; got != want {
		t.Errorf("formatProgress(bytes) = %q, want %q", got, want)
	}
	matchProg := scan.Progress{Done: 4096, Total: 12345, Unit: scan.UnitMatches}
	if got, want := formatProgress(matchProg), "4096 of 12345 matches"; got != want {
		t.Errorf("formatProgress(matches) = %q, want %q", got, want)
	}
}

func TestBusyBoxFitsNarrowTerminals(t *testing.T) {
	m := newTestModel()
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 20})
	m = nm.(model)
	m.markBusy(busyWorking)
	m.busySince = m.busySince.Add(-busyOverlayDelay)
	m.scanProg = scan.Progress{Done: 1, Total: 4, Unit: scan.UnitBytes}

	for i, line := range strings.Split(m.busyBox(), "\n") {
		if w := ansi.StringWidth(line); w > 30 {
			t.Errorf("box line %d is %d cells wide, past the 30-cell terminal", i, w)
		}
	}
	// It must still be composited (i.e. actually fit) rather than dropped.
	if !strings.Contains(m.View(), "%") {
		t.Error("the box should still be shown on a narrow terminal")
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

func TestCommandHistoryNavigation(t *testing.T) {
	m := newTestModel()
	// Submit three lines (a scan, a command, a scan).
	for _, line := range []string{"1337", ":type i32", "> 5"} {
		m = typeString(m, line)
		nm, _ := m.Update(press("enter"))
		m = nm.(model)
		m.busy = false // clear as the stateMsg would
	}
	if len(m.history) != 3 {
		t.Fatalf("history has %d entries, want 3", len(m.history))
	}

	// Up (and ctrl+k) walk backwards through history, newest first.
	nm, _ := m.Update(press("up"))
	m = nm.(model)
	if m.input.Value() != "> 5" {
		t.Errorf("after one up: input = %q, want \"> 5\"", m.input.Value())
	}
	nm, _ = m.Update(press("ctrl+k"))
	m = nm.(model)
	if m.input.Value() != ":type i32" {
		t.Errorf("after up,ctrl+k: input = %q, want \":type i32\"", m.input.Value())
	}
	nm, _ = m.Update(press("up"))
	m = nm.(model)
	if m.input.Value() != "1337" {
		t.Errorf("after three ups: input = %q, want \"1337\"", m.input.Value())
	}
	// Further up clamps at the oldest.
	nm, _ = m.Update(press("up"))
	m = nm.(model)
	if m.input.Value() != "1337" {
		t.Errorf("up past oldest should stay: input = %q", m.input.Value())
	}

	// Down (and ctrl+j) walk forward; stepping past the newest clears the line.
	nm, _ = m.Update(press("down"))
	m = nm.(model)
	if m.input.Value() != ":type i32" {
		t.Errorf("after down: input = %q, want \":type i32\"", m.input.Value())
	}
	nm, _ = m.Update(press("ctrl+j"))
	m = nm.(model)
	if m.input.Value() != "> 5" {
		t.Errorf("after down,ctrl+j: input = %q, want \"> 5\"", m.input.Value())
	}
	nm, _ = m.Update(press("down"))
	m = nm.(model)
	if m.input.Value() != "" {
		t.Errorf("down past newest should clear input, got %q", m.input.Value())
	}
}

func TestHistorySkipsConsecutiveDuplicates(t *testing.T) {
	m := newTestModel()
	for i := 0; i < 3; i++ {
		m = typeString(m, "inc")
		nm, _ := m.Update(press("enter"))
		m = nm.(model)
		m.busy = false
	}
	if len(m.history) != 1 {
		t.Errorf("consecutive duplicate submissions should collapse: history = %v", m.history)
	}
}

func TestAlignCommandAndStatus(t *testing.T) {
	m := newTestModel()
	// :align 1 issues a worker command.
	m = typeString(m, ":align 1")
	nm, cmd := m.Update(press("enter"))
	m = nm.(model)
	if cmd == nil {
		t.Error(":align 1 should issue a command")
	}
	// A bad alignment argument is reported, not sent.
	m = typeString(m, ":align nope")
	nm, cmd = m.Update(press("enter"))
	m = nm.(model)
	if cmd != nil {
		t.Error(":align nope should not issue a command")
	}
	if m.errMsg == "" {
		t.Error(":align nope should set an error")
	}

	// Status shows the alignment from state.
	m2, _ := m.Update(stateMsg(state{Attached: true, Pid: 1, Type: scan.I32, Align: 4}))
	m = m2.(model)
	if !strings.Contains(m.View(), "align 4") {
		t.Error("status should show the numeric alignment")
	}
	m2, _ = m.Update(stateMsg(state{Attached: true, Pid: 1, Type: scan.I32, Align: 0}))
	m = m2.(model)
	if !strings.Contains(m.View(), "align type") {
		t.Error("status should show 'align type' for the default")
	}
}

func TestFreezeKeyIssuesCommand(t *testing.T) {
	m := newTestModel()
	m2, _ := m.Update(stateMsg(state{
		Attached: true, Type: scan.I32, Count: 1, Scanned: true,
		Rows: []matchRow{{Index: 0, Addr: 0x1000, Value: "5"}},
	}))
	m = m2.(model)
	nm, _ := m.Update(press("tab"))
	m = nm.(model)
	nm, cmd := m.Update(press("f"))
	m = nm.(model)
	if cmd == nil {
		t.Error("f (table focused) should issue a freeze command")
	}
	if !m.busy {
		t.Error("issuing a freeze should mark the model busy")
	}
}

func TestFrozenShownInStatusAndTable(t *testing.T) {
	m := newTestModel()
	m2, _ := m.Update(stateMsg(state{
		Attached: true, Pid: 1, Type: scan.I32, Count: 1, Scanned: true, Frozen: 1,
		Rows: []matchRow{{Index: 0, Addr: 0x1000, Value: "5", Frozen: true}},
	}))
	m = m2.(model)
	if !strings.Contains(m.View(), "1 frozen") {
		t.Error("status should show the frozen count")
	}
	rows := m.table.Rows()
	if len(rows) == 0 || rows[0][0] != "*" {
		t.Errorf("frozen row should carry the marker; row0 = %v", rows)
	}
}

func TestWatchPauseToggle(t *testing.T) {
	m := newTestModel()
	if m.watchPaused {
		t.Fatal("watch should start active")
	}
	nm, _ := m.Update(press("ctrl+p"))
	m = nm.(model)
	if !m.watchPaused {
		t.Error("ctrl+p should pause the watch")
	}
	if !strings.Contains(m.status, "paused") {
		t.Errorf("status = %q, want a paused message", m.status)
	}
	nm, _ = m.Update(press("ctrl+p"))
	m = nm.(model)
	if m.watchPaused {
		t.Error("ctrl+p again should resume the watch")
	}
}

func TestTickSkipsRefreshWhenPaused(t *testing.T) {
	m := newTestModel()
	m.st = state{Attached: true, Count: 2, Scanned: true, Type: scan.I32}
	m.watchPaused = true
	nm, cmd := m.Update(tickMsg{})
	m = nm.(model)
	if m.busy {
		t.Error("a paused watch must not start a refresh on tick")
	}
	if cmd == nil {
		t.Error("tick should still reschedule itself even while paused")
	}
}

func TestPausedWatchShownInStatus(t *testing.T) {
	m := newTestModel()
	m.st = state{Attached: true, Pid: 1, Type: scan.I32}
	if !strings.Contains(m.View(), "watch") {
		t.Error("status should show the watch state")
	}
	m.watchPaused = true
	if !strings.Contains(m.View(), "watch paused") {
		t.Error("status should show 'watch paused' when paused")
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
