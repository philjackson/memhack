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

// state is a snapshot of the worker's condition, sent to the UI after every
// request so the view can be rebuilt from it.
type state struct {
	Attached bool
	Pid      int
	Type     scan.DataType
	Count    int
	Scanned  bool
	CanUndo  bool
	Frozen   int
	Align    int
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

// job is a unit of work run on the worker's locked thread.
type job struct {
	run   func(*worker) (string, error)
	reply chan state
}

// worker owns the attached process and scanner. All ptrace and /proc/<pid>/mem
// access happens on its single locked OS thread, satisfying ptrace's
// same-thread requirement while keeping the UI goroutine free.
type worker struct {
	jobs chan job
	proc *memory.Process
	sc   *scan.Scanner
	dt   scan.DataType

	// frozen maps an address to the bytes that are continually rewritten there
	// by the freeze ticker, holding its value against the target's own writes.
	frozen      map[uint64][]byte
	freezeEvery time.Duration

	// align is the scan step applied to new scanners (0 = align to type width).
	align int
}

// controller is the UI-facing handle to the worker. Its methods return
// tea.Cmds that submit a job and deliver the resulting state as a stateMsg.
type controller struct {
	jobs chan job

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
	if freezeEvery <= 0 {
		freezeEvery = defaultFreezeInterval
	}
	if align < 0 {
		align = 0
	}
	w := &worker{
		jobs:        make(chan job),
		dt:          initial,
		frozen:      map[uint64][]byte{},
		freezeEvery: freezeEvery,
		align:       align,
	}
	go w.loop()
	return &controller{jobs: w.jobs}
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

// snapshot refreshes the displayed matches and captures the current state.
func (w *worker) snapshot() state {
	st := state{Type: w.dt}
	if w.proc != nil {
		st.Attached = true
		st.Pid = w.proc.Pid
	}
	if w.sc == nil {
		return st
	}
	st.Count = len(w.sc.Matches)
	st.Scanned = w.sc.Scanned()
	st.CanUndo = w.sc.CanUndo()

	st.Frozen = len(w.frozen)
	st.Align = w.sc.Alignment()

	n := st.Count
	if n > displayLimit {
		n = displayLimit
	}
	w.sc.RefreshN(n)
	st.Rows = make([]matchRow, 0, n)
	for i := 0; i < n; i++ {
		m := w.sc.Matches[i]
		_, frozen := w.frozen[m.Addr]
		st.Rows = append(st.Rows, matchRow{Index: i, Addr: m.Addr, Value: w.dt.Format(m.Last), Frozen: frozen})
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
	w.sc = scan.NewScanner(proc, w.dt)
	w.sc.Align = w.align
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
	w.sc = scan.NewScanner(proc, w.dt)
	w.sc.Align = w.align
	return fmt.Sprintf("launched %s as pid %d", argv[0], proc.Pid), nil
}

func (w *worker) detach() {
	if w.proc != nil {
		w.proc.Close()
		w.proc = nil
	}
	w.sc = nil
	// Freezes belong to the previous process; drop them.
	w.frozen = map[uint64][]byte{}
}

// freezeIndex toggles the freeze state of match #i, capturing its current
// value when freezing.
func (w *worker) freezeIndex(i int) (string, error) {
	if w.sc == nil {
		return "", errNotAttached
	}
	if i < 0 || i >= len(w.sc.Matches) {
		return "", fmt.Errorf("no match #%d", i)
	}
	m := w.sc.Matches[i]
	if _, ok := w.frozen[m.Addr]; ok {
		delete(w.frozen, m.Addr)
		return fmt.Sprintf("unfroze %#x", m.Addr), nil
	}
	raw := make([]byte, len(m.Last))
	copy(raw, m.Last)
	w.frozen[m.Addr] = raw
	return fmt.Sprintf("froze %#x = %s", m.Addr, w.dt.Format(raw)), nil
}

// unfreezeAll clears every frozen address.
func (w *worker) unfreezeAll() (string, error) {
	n := len(w.frozen)
	w.frozen = map[uint64][]byte{}
	return fmt.Sprintf("unfroze %d address(es)", n), nil
}

func (w *worker) scanExpr(ctx context.Context, expr string) (string, error) {
	if w.sc == nil {
		return "", errNotAttached
	}
	cond, err := parseScan(expr, w.dt)
	if err != nil {
		return "", err
	}
	first := !w.sc.Scanned()
	before := len(w.sc.Matches)
	if err := w.sc.ScanContext(ctx, cond); err != nil {
		if errors.Is(err, scan.ErrCancelled) {
			// Report as a note, not an error; the match set is unchanged.
			return "scan cancelled", nil
		}
		return "", err
	}
	if first {
		return fmt.Sprintf("%d matches", len(w.sc.Matches)), nil
	}
	return fmt.Sprintf("%d matches (was %d)", len(w.sc.Matches), before), nil
}

func (w *worker) write(index int, value string) (string, error) {
	if w.sc == nil {
		return "", errNotAttached
	}
	if index < 0 || index >= len(w.sc.Matches) {
		return "", fmt.Errorf("no match #%d", index)
	}
	raw, err := w.dt.Encode(value)
	if err != nil {
		return "", err
	}
	m := w.sc.Matches[index]
	if err := w.sc.Write(m, raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s to %#x", value, m.Addr), nil
}

func (w *worker) writeAll(value string) (string, error) {
	if w.sc == nil {
		return "", errNotAttached
	}
	raw, err := w.dt.Encode(value)
	if err != nil {
		return "", err
	}
	ok, fail := 0, 0
	for _, m := range w.sc.Matches {
		if err := w.sc.Write(m, raw); err != nil {
			fail++
		} else {
			ok++
		}
	}
	return fmt.Sprintf("wrote %s to %d matches (%d failed)", value, ok, fail), nil
}

func (w *worker) undo() (string, error) {
	if w.sc == nil {
		return "", errNotAttached
	}
	if !w.sc.Undo() {
		return "nothing to undo", nil
	}
	return "undo: restored previous matches", nil
}

func (w *worker) reset() (string, error) {
	if w.sc == nil {
		return "", errNotAttached
	}
	w.sc.Reset()
	return "matches reset", nil
}

func (w *worker) setType(name string) (string, error) {
	dt, err := scan.ParseDataType(name)
	if err != nil {
		return "", err
	}
	w.dt = dt
	if w.sc != nil {
		w.sc.SetType(dt)
		w.sc.Align = w.align
	}
	return fmt.Sprintf("type = %s (matches reset)", dt), nil
}

// setAlign sets the scan step. n <= 0 means align to the type width; it takes
// effect on the next initial scan.
func (w *worker) setAlign(n int) (string, error) {
	if n < 0 {
		n = 0
	}
	w.align = n
	if w.sc != nil {
		w.sc.Align = n
	}
	if n == 0 {
		return "alignment: type width (fast)", nil
	}
	if n == 1 {
		return "alignment: every byte (thorough)", nil
	}
	return fmt.Sprintf("alignment: %d bytes", n), nil
}
