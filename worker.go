package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phil/memhack/internal/memory"
	"github.com/phil/memhack/internal/scan"
)

// displayLimit caps how many matches the worker formats for the UI and how
// many it refreshes each tick. The full match set can be enormous; the UI
// only ever shows a window of it.
const displayLimit = 500

// defaultFreezeInterval is how often frozen addresses are rewritten.
const defaultFreezeInterval = 100 * time.Millisecond

var errNotAttached = errors.New("not attached to a process")

// matchRow is one match formatted for display.
type matchRow struct {
	Index  int
	Addr   uint64
	Value  string
	Frozen bool
}

// tabInfo is one open search summarised for the tab bar.
type tabInfo struct {
	Label   string // what to show: the name, else the last scan, else a placeholder
	Name    string // the given name, empty if it has never been named
	Type    scan.DataType
	Count   int
	Scanned bool
}

// state is a snapshot of the worker's condition, sent to the UI after every
// request so the view can be rebuilt from it. Everything below the Tabs/Active
// pair describes the active tab.
type state struct {
	Attached bool
	Pid      int
	Tabs     []tabInfo
	Active   int // index into Tabs of the tab the rest of this state describes
	Type     scan.DataType
	Count    int
	Scanned  bool
	CanUndo  bool
	Frozen   int
	Align    int
	LastScan string
	Rows     []matchRow
	Note     string // human-readable result of the last action
	Err      error
}

// stateMsg carries the result of a user-initiated action; its Note/Err update
// the status line.
type stateMsg state

// refreshMsg carries a periodic value refresh. It updates the live data only,
// leaving the status/error line untouched so a prior action's message stays
// readable rather than being wiped by the next refresh tick.
type refreshMsg state

// progressMsg reports how far the running scan has got.
type progressMsg scan.Progress

// job is a unit of work run on the worker's locked thread.
type job struct {
	run   func(*worker) (string, error)
	reply chan state
}

// worker owns the attached process and the open searches. All ptrace and
// /proc/<pid>/mem access happens on its single locked OS thread, satisfying
// ptrace's same-thread requirement while keeping the UI goroutine free.
type worker struct {
	jobs chan job
	proc *memory.Process

	// tabSet holds the open searches; the active tab owns the scanner, type and
	// alignment that every request below operates on.
	tabSet

	// frozen maps an address to the bytes that are continually rewritten there
	// by the freeze ticker, holding its value against the target's own writes.
	// It is shared by every tab: freezing belongs to the target's address space,
	// so a frozen value keeps being held whichever tab is on screen.
	frozen      map[uint64][]byte
	freezeEvery time.Duration

	// prog carries scan progress to the UI. Sends are non-blocking, so a UI
	// that is slow to drain never holds up a scan (and with it the target).
	prog chan scan.Progress
}

// newWorker builds a worker with a single empty tab. freezeEvery is how often
// frozen addresses are rewritten (<= 0 uses the default).
func newWorker(initial scan.DataType, freezeEvery time.Duration, align int) *worker {
	if freezeEvery <= 0 {
		freezeEvery = defaultFreezeInterval
	}
	return &worker{
		jobs:        make(chan job),
		tabSet:      newTabSet(initial, align),
		frozen:      map[uint64][]byte{},
		freezeEvery: freezeEvery,
		prog:        make(chan scan.Progress, 1),
	}
}

// sendProgress forwards a scan progress update, dropping it if the previous one
// has not been picked up yet. Updates arrive far faster than the UI can redraw
// and progress is monotonic, so dropping costs nothing but a slightly stale bar.
func (w *worker) sendProgress(p scan.Progress) {
	select {
	case w.prog <- p:
	default:
	}
}

// controller is the UI-facing handle to the worker. Its methods return
// tea.Cmds that submit a job and deliver the resulting state as a stateMsg.
type controller struct {
	jobs chan job

	// prog receives scan progress updates for the UI to display.
	prog chan scan.Progress

	// scanCancel cancels the scan currently running (if any). It is set when a
	// scan starts and cleared when it finishes; guarded by mu because it is
	// read/written from both the UI goroutine (CancelScan) and the tea.Cmd
	// goroutine that runs the scan.
	mu         sync.Mutex
	scanCancel context.CancelFunc
}

// newController starts the worker goroutine and returns a controller for it.
// freezeEvery is how often frozen addresses are rewritten (<= 0 uses the
// default).
func newController(initial scan.DataType, freezeEvery time.Duration, align int) *controller {
	w := newWorker(initial, freezeEvery, align)
	go w.loop()
	return &controller{jobs: w.jobs, prog: w.prog}
}

// waitProgress blocks until the next scan progress update arrives. The UI
// re-issues it after each update to keep listening.
func (c *controller) waitProgress() tea.Cmd {
	if c.prog == nil {
		return nil
	}
	return func() tea.Msg { return progressMsg(<-c.prog) }
}

// CancelScan aborts the scan in progress, if one is running.
func (c *controller) CancelScan() {
	c.mu.Lock()
	cancel := c.scanCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ScanRunning reports whether a cancellable scan is currently in flight.
func (c *controller) ScanRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scanCancel != nil
}

func (w *worker) loop() {
	// Pin to one OS thread for the lifetime of the worker: ptrace binds the
	// tracer to the thread that attached, so every job must run on it.
	runtime.LockOSThread()

	// The freeze ticker rewrites frozen addresses. It runs on this same thread,
	// interleaved with jobs, so it never races the scanner's ptrace state.
	ticker := time.NewTicker(w.freezeEvery)
	defer ticker.Stop()

	for {
		select {
		case j, ok := <-w.jobs:
			if !ok {
				return
			}
			note, err := j.run(w)
			st := w.snapshot()
			st.Note = note
			st.Err = err
			j.reply <- st
		case <-ticker.C:
			w.applyFreezes()
		}
	}
}

// applyFreezes rewrites every frozen address once. It attaches only if there
// is something to freeze, so an idle target with no freezes is never touched.
func (w *worker) applyFreezes() {
	if w.proc == nil || len(w.frozen) == 0 {
		return
	}
	if err := w.proc.Attach(); err != nil {
		return
	}
	defer w.proc.Detach()
	for addr, raw := range w.frozen {
		w.proc.WriteAt(raw, addr)
	}
}

// snapshot refreshes the displayed matches and captures the current state. The
// tab bar is summarised for every tab; everything else describes the active
// one, which is all the UI shows at a time.
func (w *worker) snapshot() state {
	t := w.active()
	st := state{Type: t.dt, Active: w.cur, LastScan: t.lastScan, Frozen: len(w.frozen)}
	st.Tabs = make([]tabInfo, 0, len(w.tabs))
	for _, other := range w.tabs {
		info := tabInfo{Label: other.label(), Name: other.name, Type: other.dt}
		if other.sc != nil {
			info.Count = len(other.sc.Matches)
			info.Scanned = other.sc.Scanned()
		}
		st.Tabs = append(st.Tabs, info)
	}
	if w.proc != nil {
		st.Attached = true
		st.Pid = w.proc.Pid
	}
	if t.sc == nil {
		return st
	}
	st.Count = len(t.sc.Matches)
	st.Scanned = t.sc.Scanned()
	st.CanUndo = t.sc.CanUndo()
	st.Align = t.sc.Alignment()

	n := st.Count
	if n > displayLimit {
		n = displayLimit
	}
	// Only the active tab's values are re-read: background tabs cost the target
	// nothing until you switch to them.
	t.sc.RefreshN(n)
	st.Rows = make([]matchRow, 0, n)
	for i := 0; i < n; i++ {
		m := t.sc.Matches[i]
		_, frozen := w.frozen[m.Addr]
		st.Rows = append(st.Rows, matchRow{Index: i, Addr: m.Addr, Value: t.dt.Format(m.Last), Frozen: frozen})
	}
	return st
}

// submit wraps a worker function as a tea.Cmd.
func (c *controller) submit(run func(*worker) (string, error)) tea.Cmd {
	return func() tea.Msg {
		reply := make(chan state, 1)
		c.jobs <- job{run: run, reply: reply}
		return stateMsg(<-reply)
	}
}

func (c *controller) attach(pid int) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.attach(pid) })
}

func (c *controller) launch(argv []string) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.launch(argv) })
}

func (c *controller) scanExpr(expr string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		c.mu.Lock()
		c.scanCancel = cancel
		c.mu.Unlock()

		reply := make(chan state, 1)
		c.jobs <- job{run: func(w *worker) (string, error) { return w.scanExpr(ctx, expr) }, reply: reply}
		st := <-reply

		c.mu.Lock()
		c.scanCancel = nil
		c.mu.Unlock()
		cancel() // release context resources

		return stateMsg(st)
	}
}

func (c *controller) refresh() tea.Cmd {
	return func() tea.Msg {
		reply := make(chan state, 1)
		c.jobs <- job{run: func(*worker) (string, error) { return "", nil }, reply: reply}
		return refreshMsg(<-reply)
	}
}

func (c *controller) write(index int, value string) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.write(index, value) })
}

func (c *controller) writeAll(value string) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.writeAll(value) })
}

func (c *controller) undo() tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.undo() })
}

func (c *controller) reset() tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.reset() })
}

func (c *controller) setType(name string) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.setType(name) })
}

func (c *controller) freeze(index int) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.freezeIndex(index) })
}

func (c *controller) unfreezeAll() tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.unfreezeAll() })
}

func (c *controller) setAlign(n int) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.setAlign(n) })
}

// --- tabs ---
//
// Tab requests go through the worker like everything else, so they queue behind
// a scan that is already running rather than mutating state underneath it. A
// long scan therefore delays a tab switch until it finishes (or is cancelled
// with esc), which is the price of never racing the scanner.

func (c *controller) newTab(name string) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.newTab(name) })
}

func (c *controller) closeTab() tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.closeTab() })
}

func (c *controller) selectTab(i int) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.selectTab(i) })
}

func (c *controller) cycleTab(delta int) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.cycleTab(delta) })
}

func (c *controller) renameTab(name string) tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.renameTab(name) })
}

func (c *controller) listTabs() tea.Cmd {
	return c.submit(func(w *worker) (string, error) { return w.summary(), nil })
}

// --- worker request handlers (all run on the locked thread) ---

func (w *worker) attach(pid int) (string, error) {
	w.detach()
	proc, err := memory.Open(pid)
	if err != nil {
		return "", err
	}
	// Probe once so a permission problem surfaces now (keeping the picker on
	// screen) rather than on the first scan. Leaves the target running.
	if err := proc.Attach(); err != nil {
		proc.Close()
		return "", err
	}
	proc.Detach()

	w.proc = proc
	w.bindTabs()
	return fmt.Sprintf("attached to pid %d", pid), nil
}

func (w *worker) launch(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("no program given")
	}
	w.detach()
	proc, err := memory.Launch(argv[0], argv[1:]...)
	if err != nil {
		return "", err
	}
	if err := proc.Attach(); err != nil {
		proc.Close()
		return "", err
	}
	proc.Detach()

	w.proc = proc
	w.bindTabs()
	return fmt.Sprintf("launched %s as pid %d", argv[0], proc.Pid), nil
}

// bindTabs gives every tab a fresh scanner for the newly attached process. A
// match is an address in one process's address space and means nothing in
// another's, so every search starts over — not just the one on screen.
func (w *worker) bindTabs() {
	for _, t := range w.tabs {
		t.sc = w.newScanner(t)
	}
}

// newScanner builds a scanner for the attached process with the tab's settings
// and progress reporting wired up.
func (w *worker) newScanner(t *tab) *scan.Scanner {
	sc := scan.NewScanner(w.proc, t.dt)
	sc.Align = t.align
	if w.prog != nil {
		sc.Progress = w.sendProgress
	}
	return sc
}

func (w *worker) detach() {
	if w.proc != nil {
		w.proc.Close()
		w.proc = nil
	}
	for _, t := range w.tabs {
		t.sc = nil
	}
	// Freezes belong to the previous process; drop them.
	w.frozen = map[uint64][]byte{}
}

// newTab opens a search alongside the current one and switches to it.
func (w *worker) newTab(name string) (string, error) {
	t, err := w.open(name)
	if err != nil {
		return "", err
	}
	if w.proc != nil {
		t.sc = w.newScanner(t)
	}
	return fmt.Sprintf("tab %d of %d — %s", w.cur+1, len(w.tabs), t.label()), nil
}

// closeTab discards the current tab along with its matches.
func (w *worker) closeTab() (string, error) {
	closed, err := w.closeActive()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("closed %q; now on tab %d of %d", closed.label(), w.cur+1, len(w.tabs)), nil
}

// selectTab switches to tab i (0-based).
func (w *worker) selectTab(i int) (string, error) {
	if err := w.selectAt(i); err != nil {
		return "", err
	}
	return w.tabNote(), nil
}

// cycleTab moves delta tabs along, wrapping at both ends.
func (w *worker) cycleTab(delta int) (string, error) {
	if len(w.tabs) == 1 {
		return "only one tab open (ctrl+t opens another)", nil
	}
	w.cycle(delta)
	return w.tabNote(), nil
}

func (w *worker) renameTab(name string) (string, error) {
	w.rename(name)
	if w.active().name == "" {
		return "tab name cleared", nil
	}
	return w.tabNote(), nil
}

// tabNote describes the tab now on screen, for the status line.
func (w *worker) tabNote() string {
	t := w.active()
	if t.sc == nil || !t.sc.Scanned() {
		return fmt.Sprintf("tab %d of %d — %s (unscanned)", w.cur+1, len(w.tabs), t.label())
	}
	return fmt.Sprintf("tab %d of %d — %s (%d matches, %s)", w.cur+1, len(w.tabs), t.label(), len(t.sc.Matches), t.dt)
}

// freezeIndex toggles the freeze state of match #i in the active tab, capturing
// its current value when freezing. The freeze itself is process-wide: it goes
// on holding that address whichever tab is on screen.
func (w *worker) freezeIndex(i int) (string, error) {
	t := w.active()
	if t.sc == nil {
		return "", errNotAttached
	}
	if i < 0 || i >= len(t.sc.Matches) {
		return "", fmt.Errorf("no match #%d", i)
	}
	m := t.sc.Matches[i]
	if _, ok := w.frozen[m.Addr]; ok {
		delete(w.frozen, m.Addr)
		return fmt.Sprintf("unfroze %#x", m.Addr), nil
	}
	raw := make([]byte, len(m.Last))
	copy(raw, m.Last)
	w.frozen[m.Addr] = raw
	return fmt.Sprintf("froze %#x = %s", m.Addr, t.dt.Format(raw)), nil
}

// unfreezeAll clears every frozen address.
func (w *worker) unfreezeAll() (string, error) {
	n := len(w.frozen)
	w.frozen = map[uint64][]byte{}
	return fmt.Sprintf("unfroze %d address(es)", n), nil
}

func (w *worker) scanExpr(ctx context.Context, expr string) (string, error) {
	t := w.active()
	if t.sc == nil {
		return "", errNotAttached
	}
	cond, err := parseScan(expr, t.dt)
	if err != nil {
		return "", err
	}
	// Remembered per tab: an empty enter repeats this tab's own last scan, and
	// an unnamed tab is labelled by it.
	t.lastScan = expr
	first := !t.sc.Scanned()
	before := len(t.sc.Matches)
	if err := t.sc.ScanContext(ctx, cond); err != nil {
		if errors.Is(err, scan.ErrCancelled) {
			// Report as a note, not an error; the match set is unchanged.
			return "scan cancelled", nil
		}
		return "", err
	}
	if first {
		return fmt.Sprintf("%d matches", len(t.sc.Matches)), nil
	}
	return fmt.Sprintf("%d matches (was %d)", len(t.sc.Matches), before), nil
}

func (w *worker) write(index int, value string) (string, error) {
	t := w.active()
	if t.sc == nil {
		return "", errNotAttached
	}
	if index < 0 || index >= len(t.sc.Matches) {
		return "", fmt.Errorf("no match #%d", index)
	}
	raw, err := t.dt.Encode(value)
	if err != nil {
		return "", err
	}
	m := t.sc.Matches[index]
	if err := t.sc.Write(m, raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s to %#x", value, m.Addr), nil
}

func (w *worker) writeAll(value string) (string, error) {
	t := w.active()
	if t.sc == nil {
		return "", errNotAttached
	}
	raw, err := t.dt.Encode(value)
	if err != nil {
		return "", err
	}
	ok, fail := 0, 0
	for _, m := range t.sc.Matches {
		if err := t.sc.Write(m, raw); err != nil {
			fail++
		} else {
			ok++
		}
	}
	return fmt.Sprintf("wrote %s to %d matches (%d failed)", value, ok, fail), nil
}

func (w *worker) undo() (string, error) {
	t := w.active()
	if t.sc == nil {
		return "", errNotAttached
	}
	if !t.sc.Undo() {
		return "nothing to undo", nil
	}
	return "undo: restored previous matches", nil
}

func (w *worker) reset() (string, error) {
	t := w.active()
	if t.sc == nil {
		return "", errNotAttached
	}
	t.sc.Reset()
	return "matches reset", nil
}

// setType changes the active tab's data type, resetting that tab's matches.
// Other tabs keep their own type and matches.
func (w *worker) setType(name string) (string, error) {
	dt, err := scan.ParseDataType(name)
	if err != nil {
		return "", err
	}
	t := w.active()
	t.dt = dt
	if t.sc != nil {
		t.sc.SetType(dt)
		t.sc.Align = t.align
	}
	return fmt.Sprintf("type = %s (matches reset)", dt), nil
}

// setAlign sets the active tab's scan step. n <= 0 means align to the type
// width; it takes effect on the next initial scan.
func (w *worker) setAlign(n int) (string, error) {
	if n < 0 {
		n = 0
	}
	t := w.active()
	t.align = n
	if t.sc != nil {
		t.sc.Align = n
	}
	if n == 0 {
		return "alignment: type width (fast)", nil
	}
	if n == 1 {
		return "alignment: every byte (thorough)", nil
	}
	return fmt.Sprintf("alignment: %d bytes", n), nil
}
