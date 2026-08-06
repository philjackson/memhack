package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/phil/memhack/internal/scan"
)

const (
	// defaultWatchInterval is how often the live watch re-reads values. Each
	// tick attaches, reads, and detaches, so the target is only briefly
	// stopped per tick and runs freely in between; a modest interval keeps it
	// unintrusive (especially for large, multithreaded targets).
	defaultWatchInterval = 1 * time.Second
	scanPrompt           = "scan› "
	scanPlaceholder      = "e.g. 1337, > 100, 10..20, inc, :type f32 — f1 for help"

	// busyOverlayDelay is how long a request must be in flight before the
	// centred progress box appears. Quick actions (writes, freezes, a type
	// change) finish well inside it, so the box never flashes for them; only
	// work slow enough to look like a hang draws it.
	busyOverlayDelay = 250 * time.Millisecond

	// Labels for in-flight work, shown in the progress box.
	busyWorking    = "working"
	busyCancelling = "cancelling scan"

	// busyBoxWidth is the inner width of the progress box, in cells. It is
	// narrowed to fit when the terminal is too small for it.
	busyBoxWidth = 38

	// tabLabelWidth caps how much of a tab's label the tab bar shows, so one
	// long scan expression can't crowd the other tabs off the line.
	tabLabelWidth = 14
)

// inputMode selects how the text input's contents are interpreted on Enter.
type inputMode int

const (
	modeScan   inputMode = iota // a scan expression (or ":" command)
	modeWrite                   // a value to write to the selected match
	modeRename                  // a new name for the active tab
)

// tickMsg drives the periodic refresh of displayed values.
type tickMsg time.Time

// screen selects which view is active.
type screen int

const (
	screenScanner screen = iota // the matches table + scan input
	screenPicker                // the process picker
	screenHelp                  // the scrollable key/command reference
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("57")).Padding(0, 1)
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	// Styles for the centred progress box.
	busyBoxStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("57")).Padding(0, 2)
	busyLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229"))

	// Styles for the tab bar and the help screen.
	tabStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)
	activeTabStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("57")).Padding(0, 1)
	helpHeadStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	helpKeyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("229"))
)

type model struct {
	ctrl *controller

	input textinput.Model
	table table.Model
	spin  spinner.Model
	bar   progress.Model
	list  list.Model
	help  viewport.Model

	screen        screen
	pendingAttach bool

	st         state
	mode       inputMode
	writeIdx   int
	focusTable bool
	busy       bool
	busyLabel  string        // what the in-flight work is called; "" = show no overlay
	busySince  time.Time     // when the current busy stretch began
	scanProg   scan.Progress // how far the running scan has got (zero Total = unknown)
	status     string
	errMsg     string
	lastScan   string   // the active tab's last scan expression, for instant repeat
	history    []string // submitted scan expressions and commands, oldest first (shared by all tabs)
	histPos    int      // cursor into history; == len(history) means "fresh line"

	watchInterval time.Duration
	watchPaused   bool

	width, height int
	startup       tea.Cmd
}

func newModel(ctrl *controller, dt scan.DataType, startup tea.Cmd, start screen, watch time.Duration) model {
	if watch <= 0 {
		watch = defaultWatchInterval
	}
	ti := textinput.New()
	ti.Prompt = scanPrompt
	ti.Placeholder = scanPlaceholder
	ti.CharLimit = 128
	ti.Focus()

	cols := []table.Column{
		{Title: "❄", Width: 2},
		{Title: "#", Width: 6},
		{Title: "Address", Width: 18},
		{Title: "Value", Width: 20},
	}
	tbl := table.New(table.WithColumns(cols), table.WithHeight(10))
	s := table.DefaultStyles()
	s.Header = s.Header.Bold(true).Foreground(lipgloss.Color("63"))
	s.Selected = s.Selected.Foreground(lipgloss.Color("229")).Background(lipgloss.Color("57")).Bold(true)
	tbl.SetStyles(s)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))

	// ViewAs renders the bar straight from a fraction, so the progress model
	// needs no animation frames of its own.
	bar := progress.New(progress.WithDefaultGradient(), progress.WithWidth(busyBoxWidth))

	return model{
		ctrl:  ctrl,
		input: ti,
		table: tbl,
		spin:  sp,
		bar:   bar,
		list:  newProcList(),
		help:  viewport.New(80, 20),
		// The worker's first reply fills the tab bar in; until then, show the
		// single tab it starts with rather than an empty strip.
		st:            state{Type: dt, Tabs: []tabInfo{{Label: "empty", Type: dt}}},
		screen:        start,
		mode:          modeScan,
		startup:       startup,
		watchInterval: watch,
	}
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, tickCmd(m.watchInterval)}
	if c := m.ctrl.waitProgress(); c != nil {
		cmds = append(cmds, c)
	}
	if m.startup != nil {
		cmds = append(cmds, m.startup)
	}
	if m.screen == screenPicker {
		cmds = append(cmds, loadProcs)
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.list.SetSize(msg.Width, msg.Height-1)
		return m, nil

	case procListMsg:
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			return m, nil
		}
		return m, m.setProcs(msg.procs)

	case tickMsg:
		cmds := []tea.Cmd{tickCmd(m.watchInterval)}
		// Auto-refresh only when the watch is running, we're idle, and there
		// are matches to read. Each refresh attaches, reads, and detaches, so
		// while paused (or matchless) the target is never touched.
		if !m.watchPaused && !m.busy && m.st.Attached && m.st.Count > 0 {
			// No label: the watch fires every interval, so it animates the
			// status-line spinner but never draws the centred box.
			cmds = append(cmds, m.markBusy(""), m.ctrl.refresh())
		}
		return m, tea.Batch(cmds...)

	case progressMsg:
		// Keep listening whatever we do with this update: the channel outlives
		// any single scan.
		cmd := m.ctrl.waitProgress()
		// Updates that outlive their scan (a last one landing after the reply)
		// would otherwise leave a stale bar behind for the next one.
		if m.busy {
			m.scanProg = scan.Progress(msg)
		}
		return m, cmd

	case spinner.TickMsg:
		// Keep the spinner animating only while a request is in flight; once
		// the reply lands (busy cleared) the tick loop ends.
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case refreshMsg:
		// Live value refresh: update the data but keep the status/error line,
		// so a prior action's message stays on screen instead of flashing away.
		// A labelled action issued while this refresh was in flight is still
		// running behind it (the worker runs jobs one at a time), so leave the
		// busy state to that action's reply rather than clearing it here.
		if m.busyLabel == "" {
			m.clearBusy()
		}
		m.adoptState(state(msg))
		m.refreshTable()
		return m, nil

	case stateMsg:
		m.clearBusy()
		m.adoptState(state(msg))
		if m.st.Err != nil {
			m.errMsg = m.st.Err.Error()
		} else {
			m.errMsg = ""
			if m.st.Note != "" {
				m.status = m.st.Note
			}
		}
		// A selection from the picker only leaves the picker once the attach
		// actually succeeds; on failure we stay so the user can pick again.
		if m.pendingAttach {
			m.pendingAttach = false
			if m.st.Err == nil && m.st.Attached {
				m.screen = screenScanner
			}
		}
		m.refreshTable()
		return m, nil

	case tea.KeyMsg:
		switch m.screen {
		case screenPicker:
			return m.handlePickerKey(msg)
		case screenHelp:
			return m.handleHelpKey(msg)
		}
		return m.handleKey(msg)
	}

	// Route anything else (e.g. cursor blink) to the focused component.
	var cmd tea.Cmd
	if m.screen == screenPicker {
		m.list, cmd = m.list.Update(msg)
	} else if m.focusTable {
		m.table, cmd = m.table.Update(msg)
	} else {
		m.input, cmd = m.input.Update(msg)
	}
	return m, cmd
}

// adoptState takes a new worker snapshot. Anything the UI mirrors from the
// worker — which tab is active, and that tab's last scan — is picked up here,
// so switching tabs swaps the repeatable scan along with the matches.
func (m *model) adoptState(st state) {
	switched := st.Active != m.st.Active || len(st.Tabs) != len(m.st.Tabs)
	m.st = st
	m.lastScan = st.LastScan
	if switched {
		// The selection belonged to the tab we've left; start at the top.
		m.table.SetCursor(0)
	}
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+d":
		// Closes the tab, bash-style: only on an empty line. With something
		// typed it falls through to the input, where ctrl+d keeps its usual
		// meaning of deleting the character under the cursor.
		if m.input.Value() == "" {
			return m.tabAction(m.ctrl.closeTab())
		}
	case "ctrl+z":
		return m.issue(m.ctrl.undo())
	case "ctrl+r":
		return m.issue(m.ctrl.reset())
	case "f1":
		return m.openHelp()
	case "f2":
		return m.startRename()
	case "ctrl+t", "alt+t":
		return m.tabAction(m.ctrl.newTab(""))
	case "alt+w":
		return m.tabAction(m.ctrl.closeTab())
	case "shift+tab", "alt+right":
		return m.tabAction(m.ctrl.cycleTab(+1))
	case "alt+left":
		return m.tabAction(m.ctrl.cycleTab(-1))
	case "ctrl+p":
		m.watchPaused = !m.watchPaused
		if m.watchPaused {
			m.status = "live watch paused"
		} else {
			m.status = "live watch resumed"
		}
		return m, nil
	case "tab":
		m.toggleFocus()
		return m, nil
	case "esc":
		if m.mode != modeScan {
			m.cancelInput()
			return m, nil
		}
		// Cancel an in-progress scan.
		if m.busy && m.ctrl.ScanRunning() {
			m.ctrl.CancelScan()
			m.status = "cancelling scan…"
			// Say so in the progress box too: cancelling isn't instant, and the
			// box is what the eye is on while the scan winds down.
			m.busyLabel = busyCancelling
		}
		return m, nil
	}

	// alt+<digit> jumps straight to a tab.
	if n, ok := altDigit(msg); ok {
		return m.tabAction(m.ctrl.selectTab(n - 1))
	}

	if m.focusTable {
		return m.handleTableKey(msg)
	}
	return m.handleInputKey(msg)
}

// altDigit reports the digit of an alt+<1-9> key press.
func altDigit(msg tea.KeyMsg) (int, bool) {
	if msg.Type != tea.KeyRunes || !msg.Alt || len(msg.Runes) != 1 {
		return 0, false
	}
	if r := msg.Runes[0]; r >= '1' && r <= '9' {
		return int(r - '0'), true
	}
	return 0, false
}

// tabAction issues a tab command. It leaves write mode first: a pending write
// is aimed at a match index in the tab being left, which would mean something
// quite different in the tab being switched to.
func (m model) tabAction(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mode == modeWrite {
		m.resetInputToScan()
	}
	return m.issue(cmd)
}

// openHelp shows the key/command reference.
func (m model) openHelp() (tea.Model, tea.Cmd) {
	m.help.SetContent(helpBody())
	m.help.GotoTop()
	m.screen = screenHelp
	return m, nil
}

// handleHelpKey drives the help screen: anything that isn't a scroll closes it.
func (m model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "up", "down", "pgup", "pgdown", "home", "end", "k", "j", "ctrl+k", "ctrl+j":
		var cmd tea.Cmd
		m.help, cmd = m.help.Update(msg)
		return m, cmd
	}
	m.screen = screenScanner
	return m, nil
}

func (m model) handleTableKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "w", "enter":
		if len(m.st.Rows) == 0 {
			return m, nil
		}
		cur := m.table.Cursor()
		if cur < 0 {
			cur = 0
		}
		if cur >= len(m.st.Rows) {
			return m, nil
		}
		m.writeIdx = m.st.Rows[cur].Index
		m.mode = modeWrite
		m.focusTable = false
		m.table.Blur()
		m.input.Reset()
		m.input.Prompt = fmt.Sprintf("write #%d ‹%s› = ", m.writeIdx, m.st.Rows[cur].Value)
		m.input.Focus()
		return m, textinput.Blink
	case "f":
		// Toggle freezing the selected match at its current value.
		if len(m.st.Rows) == 0 {
			return m, nil
		}
		cur := m.table.Cursor()
		if cur < 0 {
			cur = 0
		}
		if cur >= len(m.st.Rows) {
			return m, nil
		}
		return m.issue(m.ctrl.freeze(m.st.Rows[cur].Index))
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+k":
		if m.mode == modeScan {
			m.historyStep(-1)
			return m, nil
		}
	case "down", "ctrl+j":
		if m.mode == modeScan {
			m.historyStep(+1)
			return m, nil
		}
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		if m.mode == modeRename {
			// An empty name is meaningful here: it clears the name, so the tab
			// goes back to being labelled by its last scan.
			m.resetInputToScan()
			return m.issue(m.ctrl.renameTab(v))
		}
		if v == "" {
			// Instant repeat: re-run the last scan without retyping it. Handy
			// for narrowing (inc/dec/changed) as a value keeps changing.
			if m.mode == modeScan && m.lastScan != "" {
				m.status = "repeat: " + m.lastScan
				return m.issue(m.ctrl.scanExpr(m.lastScan))
			}
			return m, nil
		}
		if m.mode == modeWrite {
			idx := m.writeIdx
			m.resetInputToScan() // the value is already captured in v
			return m.issue(m.ctrl.write(idx, v))
		}
		m.addHistory(v)
		m.input.Reset()
		if strings.HasPrefix(v, ":") {
			return m.command(strings.TrimSpace(v[1:]))
		}
		switch v {
		case "quit", "exit", "q":
			return m, tea.Quit
		}
		m.lastScan = v
		return m.issue(m.ctrl.scanExpr(v))
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handlePickerKey drives the process picker screen.
func (m model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While the "/" filter is active, hand everything to the list (typing,
	// enter to apply, esc to clear).
	if m.list.FilterState() == list.Filtering {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	// No ctrl+d here either: reopening the picker from a session with several
	// tabs open must not put a whole-app quit under the close-tab key.
	case "ctrl+c", "q":
		return m, tea.Quit
	case "ctrl+r":
		return m, loadProcs
	case "enter":
		if it, ok := m.list.SelectedItem().(procItem); ok {
			m.pendingAttach = true
			return m.issue(m.ctrl.attach(it.Pid))
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// openPicker switches to the process picker and reloads the process list.
func (m model) openPicker() (tea.Model, tea.Cmd) {
	m.screen = screenPicker
	return m, loadProcs
}

// command handles a ":" command line.
func (m model) command(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return m, nil
	}
	switch fields[0] {
	case "q", "quit", "exit":
		return m, tea.Quit
	case "help", "h", "?":
		return m.openHelp()
	case "ps", "procs":
		return m.openPicker()
	case "tab", "tabs":
		return m.tabCommand(fields[1:])
	case "pid", "attach":
		if len(fields) != 2 {
			m.errMsg = "usage: :pid <pid>"
			return m, nil
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			m.errMsg = "invalid pid: " + fields[1]
			return m, nil
		}
		return m.issue(m.ctrl.attach(pid))
	case "run":
		if len(fields) < 2 {
			m.errMsg = "usage: :run <prog> [args...]"
			return m, nil
		}
		return m.issue(m.ctrl.launch(fields[1:]))
	case "type":
		if len(fields) != 2 {
			m.errMsg = "usage: :type <i8..u64|f32|f64|bytes|string>"
			return m, nil
		}
		return m.issue(m.ctrl.setType(fields[1]))
	case "set":
		if len(fields) != 3 {
			m.errMsg = "usage: :set <index> <value>"
			return m, nil
		}
		idx, err := strconv.Atoi(fields[1])
		if err != nil {
			m.errMsg = "invalid index: " + fields[1]
			return m, nil
		}
		return m.issue(m.ctrl.write(idx, fields[2]))
	case "setall":
		if len(fields) != 2 {
			m.errMsg = "usage: :setall <value>"
			return m, nil
		}
		return m.issue(m.ctrl.writeAll(fields[1]))
	case "reset":
		return m.issue(m.ctrl.reset())
	case "undo":
		return m.issue(m.ctrl.undo())
	case "freeze":
		if len(fields) != 2 {
			m.errMsg = "usage: :freeze <index>"
			return m, nil
		}
		idx, err := strconv.Atoi(fields[1])
		if err != nil {
			m.errMsg = "invalid index: " + fields[1]
			return m, nil
		}
		return m.issue(m.ctrl.freeze(idx))
	case "unfreeze":
		return m.issue(m.ctrl.unfreezeAll())
	case "align":
		if len(fields) != 2 {
			m.errMsg = "usage: :align <n|type> (0/type = type width, 1 = every byte)"
			return m, nil
		}
		n, err := parseAlign(fields[1])
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		return m.issue(m.ctrl.setAlign(n))
	default:
		m.errMsg = "unknown command: " + fields[0]
		return m, nil
	}
}

// tabCommand handles ":tab ...", the typed equivalent of the tab keys. It
// accepts exactly the forms the REPL's "tab" command does (see tabUsage).
func (m model) tabCommand(args []string) (tea.Model, tea.Cmd) {
	cmd, err := parseTabCmd(args)
	if err != nil {
		m.errMsg = err.Error()
		return m, nil
	}
	switch cmd.action {
	case tabNew:
		return m.tabAction(m.ctrl.newTab(cmd.name))
	case tabClose:
		return m.tabAction(m.ctrl.closeTab())
	case tabRename:
		return m.issue(m.ctrl.renameTab(cmd.name))
	case tabSelect:
		return m.tabAction(m.ctrl.selectTab(cmd.index))
	}
	return m.issue(m.ctrl.listTabs())
}

// issue marks the model busy and runs a worker command.
func (m model) issue(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if cmd == nil {
		return m, nil
	}
	return m, tea.Batch(cmd, m.markBusy(busyWorking))
}

// maxInputHistory bounds how many entered lines are remembered.
const maxInputHistory = 200

// addHistory records a submitted line (scan expression or command) and resets
// the browse cursor to the fresh-line position.
func (m *model) addHistory(line string) {
	if n := len(m.history); n == 0 || m.history[n-1] != line {
		m.history = append(m.history, line)
		if len(m.history) > maxInputHistory {
			m.history = m.history[len(m.history)-maxInputHistory:]
		}
	}
	m.histPos = len(m.history)
}

// historyStep moves through the entered-command history and loads the entry
// into the input. dir < 0 goes older (up), dir > 0 goes newer (down); stepping
// past the newest entry returns to an empty fresh line.
func (m *model) historyStep(dir int) {
	if len(m.history) == 0 {
		return
	}
	pos := m.histPos + dir
	if pos < 0 {
		pos = 0
	}
	if pos >= len(m.history) {
		m.histPos = len(m.history)
		m.input.SetValue("")
		return
	}
	m.histPos = pos
	m.input.SetValue(m.history[pos])
	m.input.CursorEnd()
}

// markBusy flips the busy flag and, on the idle→busy edge, returns the command
// that starts the spinner animation. It returns nil if already busy, so the
// spinner's tick loop is never started twice concurrently.
//
// label names the work for the centred progress box; "" means "show no box"
// (used by the live watch, which is routine and shouldn't be announced).
func (m *model) markBusy(label string) tea.Cmd {
	if m.busy {
		// An action issued while an unlabelled refresh is in flight takes over
		// the indicator, timed from when the action itself started.
		if label != "" && m.busyLabel == "" {
			m.busyLabel = label
			m.busySince = time.Now()
			m.scanProg = scan.Progress{}
		}
		return nil
	}
	m.busy = true
	m.busyLabel = label
	m.busySince = time.Now()
	m.scanProg = scan.Progress{}
	return m.spin.Tick
}

// clearBusy marks the in-flight work as finished, which stops the spinner tick
// loop and removes the progress box.
func (m *model) clearBusy() {
	m.busy = false
	m.busyLabel = ""
	m.scanProg = scan.Progress{}
}

func (m *model) toggleFocus() {
	m.focusTable = !m.focusTable
	if m.focusTable {
		m.input.Blur()
		m.table.Focus()
	} else {
		m.table.Blur()
		if m.mode != modeScan {
			m.resetInputToScan()
		}
		m.input.Focus()
	}
}

// startRename turns the input into a rename prompt for the active tab,
// prefilled with its current name so it can be edited rather than retyped.
func (m model) startRename() (tea.Model, tea.Cmd) {
	m.mode = modeRename
	m.focusTable = false
	m.table.Blur()
	m.input.Reset()
	m.input.Prompt = fmt.Sprintf("name tab %d › ", m.st.Active+1)
	m.input.Placeholder = "a name for this search — empty clears it"
	if name := m.activeTabName(); name != "" {
		m.input.SetValue(name)
		m.input.CursorEnd()
	}
	m.input.Focus()
	return m, textinput.Blink
}

// activeTabName is the active tab's given name, empty if it has never been
// named (its label then comes from its last scan).
func (m model) activeTabName() string {
	if m.st.Active < 0 || m.st.Active >= len(m.st.Tabs) {
		return ""
	}
	return m.st.Tabs[m.st.Active].Name
}

// cancelInput abandons whatever the input was collecting and returns it to
// scanning.
func (m *model) cancelInput() {
	what := "write"
	if m.mode == modeRename {
		what = "rename"
	}
	m.resetInputToScan()
	m.status = what + " cancelled"
}

func (m *model) resetInputToScan() {
	m.mode = modeScan
	m.input.Reset()
	m.input.Prompt = scanPrompt
	m.input.Placeholder = scanPlaceholder
}

func (m *model) refreshTable() {
	rows := make([]table.Row, 0, len(m.st.Rows))
	for _, r := range m.st.Rows {
		mark := ""
		if r.Frozen {
			mark = "*"
		}
		rows = append(rows, table.Row{
			mark,
			strconv.Itoa(r.Index),
			fmt.Sprintf("%#012x", r.Addr),
			r.Value,
		})
	}
	m.table.SetRows(rows)
	// bubbles' table leaves the cursor at -1 until it is navigated; seed it so
	// the first row is selectable (and editable) immediately.
	if len(rows) > 0 && m.table.Cursor() < 0 {
		m.table.SetCursor(0)
	}
}

func (m *model) layout() {
	if m.width <= 0 {
		return
	}
	m.input.Width = m.width - 12

	frzW, idxW, addrW := 2, 6, 18
	valW := m.width - frzW - idxW - addrW - 12
	if valW < 10 {
		valW = 10
	}
	m.table.SetColumns([]table.Column{
		{Title: "❄", Width: frzW},
		{Title: "#", Width: idxW},
		{Title: "Address", Width: addrW},
		{Title: "Value", Width: valW},
	})
	m.table.SetWidth(m.width - 2)

	// The help screen fills the terminal below its own title line.
	m.help.Width = m.width
	if hh := m.height - 3; hh > 1 {
		m.help.Height = hh
	}

	// Reserve lines for title, tab bar, status, input, message, and help.
	h := m.height - 10
	if h < 3 {
		h = 3
	}
	m.table.SetHeight(h)
}

func (m model) View() string {
	switch m.screen {
	case screenPicker:
		return m.overlay(m.pickerView())
	case screenHelp:
		return m.helpView()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("memhack") + "  " + m.statusLine() + "\n")
	b.WriteString(m.tabBar() + "\n\n")
	b.WriteString(m.table.View() + "\n")
	b.WriteString(m.matchFooter() + "\n\n")
	b.WriteString(m.input.View() + "\n")

	switch {
	case m.errMsg != "":
		b.WriteString(errStyle.Render("✗ "+m.errMsg) + "\n")
	case m.status != "":
		b.WriteString(okStyle.Render("• "+m.status) + "\n")
	default:
		b.WriteString("\n")
	}
	b.WriteString(helpStyle.Render(m.helpText()))
	return m.overlay(b.String())
}

// overlay floats the progress box over the middle of the view while work that
// is slow enough to notice is in flight. The status-line spinner covers the
// short stretch before it appears.
func (m model) overlay(view string) string {
	if !m.showBusyBox() {
		return view
	}
	return placeOverlayCenter(view, m.busyBox(), m.width)
}

// showBusyBox reports whether the centred progress box is due.
func (m model) showBusyBox() bool {
	return m.busy && m.busyLabel != "" && time.Since(m.busySince) >= busyOverlayDelay
}

// busyBox renders the centred progress box: a spinner, what is running, how far
// it has got, how long it has been running, and how to stop it if it can be
// stopped. A scan reports real progress, so it gets a bar; anything else is
// quick and indeterminate, so it gets the spinner alone.
func (m model) busyBox() string {
	label, hint := m.busyLabel+"…", ""
	if m.busyLabel == busyWorking && m.ctrl != nil && m.ctrl.ScanRunning() {
		label, hint = "scanning memory…", "esc: cancel"
	}
	spin := m.spin.View() + " " + busyLabelStyle.Render(label)
	elapsed := statusStyle.Render(fmt.Sprintf("%.1fs", time.Since(m.busySince).Seconds()))

	// Without a bar there is nothing to line up with, so the box shrinks to its
	// contents rather than sitting mostly empty.
	p := m.scanProg
	if p.Total == 0 {
		body := spin + "  " + elapsed
		if hint != "" {
			body = lipgloss.JoinVertical(lipgloss.Center, body, helpStyle.Render(hint))
		}
		return busyBoxStyle.Render(body)
	}

	w := m.busyBoxWidth()
	bar := m.bar
	bar.Width = w
	return busyBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
		padBetween(spin, elapsed, w),
		bar.ViewAs(p.Fraction()),
		padBetween(statusStyle.Render(formatProgress(p)), helpStyle.Render(hint), w),
	))
}

// busyBoxWidth is the box's inner width, shrunk to fit a narrow terminal.
func (m model) busyBoxWidth() int {
	// The border and padding around the content cost 6 cells; leave a little
	// air either side of the box.
	if max := m.width - 10; m.width > 0 && max < busyBoxWidth {
		if max < 12 {
			return 12
		}
		return max
	}
	return busyBoxWidth
}

// formatProgress renders a progress update's counters, in whichever unit the
// scan is measured in.
func formatProgress(p scan.Progress) string {
	if p.Unit == scan.UnitBytes {
		return fmt.Sprintf("%s of %s", humanBytes(p.Done), humanBytes(p.Total))
	}
	return fmt.Sprintf("%d of %d %s", p.Done, p.Total, p.Unit)
}

// tabBar renders the open searches, the active one highlighted, with the key
// that opens another on the right. It is drawn even with a single tab open, so
// the feature is visible rather than hidden behind a keystroke nobody tries.
func (m model) tabBar() string {
	if len(m.st.Tabs) == 0 {
		return ""
	}
	chips := make([]string, 0, len(m.st.Tabs))
	for i, t := range m.st.Tabs {
		text := fmt.Sprintf("%d %s", i+1, truncateLabel(t.Label, tabLabelWidth))
		if t.Scanned {
			text += fmt.Sprintf(" (%d)", t.Count)
		}
		if i == m.st.Active {
			chips = append(chips, activeTabStyle.Render(text))
			continue
		}
		chips = append(chips, tabStyle.Render(text))
	}
	line := strings.Join(chips, " ")
	if m.width <= 0 {
		return line
	}
	if w := ansi.StringWidth(line); w > m.width {
		// Too many tabs to lay out at this width: name the current one instead.
		cur := m.st.Tabs[m.st.Active]
		return statusStyle.Render(fmt.Sprintf("tab %d/%d", m.st.Active+1, len(m.st.Tabs))) +
			" " + activeTabStyle.Render(truncateLabel(cur.Label, tabLabelWidth))
	}
	// Show the fullest hint the width allows, shedding detail as the tabs take
	// up more of the line. Switching is only worth mentioning once there is
	// somewhere to switch to.
	hints := []string{"ctrl+t: new tab · f2: rename", "ctrl+t: new tab"}
	if len(m.st.Tabs) > 1 {
		hints = []string{
			"shift+tab/alt+N: switch · ctrl+t: new · f2: rename",
			"shift+tab/alt+N: switch · ctrl+t: new",
			"shift+tab: switch",
		}
	}
	for _, h := range hints {
		hint := helpStyle.Render(h)
		if ansi.StringWidth(line)+ansi.StringWidth(hint)+2 <= m.width {
			return padBetween(line, hint, m.width)
		}
	}
	return line
}

// truncateLabel shortens a tab label to at most w cells, marking the cut.
func truncateLabel(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// helpView renders the key/command reference over the whole screen. The scroll
// percentage is shown so it's clear there is more below the fold.
func (m model) helpView() string {
	title := titleStyle.Render("memhack help") + "  " +
		statusStyle.Render(fmt.Sprintf("↑/↓ scroll (%.0f%%) · any other key returns", m.help.ScrollPercent()*100))
	return title + "\n\n" + m.help.View()
}

// helpBody is the reference the help screen scrolls through: every key and
// command memhack understands, kept in step with handleKey and command().
func helpBody() string {
	var b strings.Builder
	section := func(title string) { b.WriteString(helpHeadStyle.Render(title) + "\n") }
	row := func(keys, what string) {
		b.WriteString("  " + helpKeyStyle.Render(padRight(keys, 20)) + what + "\n")
	}
	note := func(text string) { b.WriteString("  " + helpStyle.Render(text) + "\n") }
	blank := func() { b.WriteString("\n") }

	section("Scanning")
	row("<expr> enter", "scan: 1337 · > 100 · 10..20 · changed · inc · dec 5")
	row("enter (empty)", "repeat this tab's last scan")
	row("↑ / ↓", "recall earlier entries (also ctrl+k / ctrl+j)")
	row("esc", "cancel the scan in progress; matches are left alone")
	row("ctrl+z / ctrl+r", "undo the last scan / reset this tab's matches")
	row("ctrl+p", "pause or resume the live value watch")
	blank()

	section("Tabs — one independent search each")
	row("ctrl+t", "open a new tab (inherits the current type and alignment)")
	row("ctrl+d / alt+w", "close this tab, discarding its matches")
	note("ctrl+d closes only on an empty line; with something typed it deletes")
	note("the character under the cursor, as it does in a shell.")
	row("shift+tab", "next tab (alt+← / alt+→ move either way)")
	row("alt+1 … alt+9", "jump to tab N")
	row("f2", "rename this tab; an empty name clears it again")
	row(":tab rename <name>", "the same, typed; unnamed tabs show their last scan")
	row(":tab", "list the open tabs (:tab new, :tab close, :tab <n>)")
	note("Each tab keeps its own matches, undo history, type and alignment.")
	note("The target process and its frozen addresses are shared by all tabs,")
	note("so a frozen value keeps being held while you work in another tab.")
	note("Attaching to a process clears every tab. Up to 9 tabs; a tab switch")
	note("waits for a running scan to finish (esc cancels it).")
	blank()

	section("Matches")
	row("tab", "move focus between the scan input and the table")
	row("↑ / ↓", "select a match (table focused)")
	row("w or enter", "write a new value to the selected match")
	row("f", "freeze / unfreeze it at its current value")
	note("Frozen addresses are rewritten on an interval (-freeze, default 100ms)")
	note("and marked * in the table. Set a value first, then freeze, to pin it.")
	blank()

	section("Commands — type them into the scan input")
	row(":pid <pid>", "attach to a running process")
	row(":run <prog> [args]", "launch a program and attach to it")
	row(":ps", "reopen the process picker")
	row(":type <t>", "i8…u64, f32, f64, bytes, string (resets this tab)")
	row(":align <n|type>", "1 = every byte (thorough), type = type width (fast)")
	row(":set <i> <value>", "write to match #i")
	row(":setall <value>", "write to every match in this tab")
	row(":freeze <i>", "freeze match #i; :unfreeze clears every freeze")
	row(":undo / :reset", "same as ctrl+z / ctrl+r")
	row(":help", "this screen (also f1)")
	row(":q", "quit memhack (also quit, ctrl+c)")

	return b.String()
}

// pickerView renders the process picker. The list component draws its own
// title, items, pagination, filter, and help line.
func (m model) pickerView() string {
	view := m.list.View()
	if m.errMsg != "" {
		view += "\n" + errStyle.Render("✗ "+m.errMsg)
	}
	return view
}

func (m model) statusLine() string {
	target := "no target"
	if m.st.Attached {
		target = fmt.Sprintf("pid %d", m.st.Pid)
	}
	matches := "unscanned"
	if m.st.Scanned {
		matches = fmt.Sprintf("%d matches", m.st.Count)
	}
	watch := fmt.Sprintf("watch %s", m.watchInterval)
	if m.watchPaused {
		watch = "watch paused"
	}
	align := "align type"
	if m.st.Align > 0 {
		align = fmt.Sprintf("align %d", m.st.Align)
	}
	frozen := ""
	if m.st.Frozen > 0 {
		frozen = fmt.Sprintf(" │ %d frozen", m.st.Frozen)
	}
	line := statusStyle.Render(fmt.Sprintf("%s │ type %s │ %s │ %s │ %s%s", target, m.st.Type, matches, align, watch, frozen))
	if m.busy {
		line += "  " + m.spin.View() + statusStyle.Render(" working…")
	}
	return line
}

func (m model) matchFooter() string {
	if m.st.Count > len(m.st.Rows) {
		return statusStyle.Render(fmt.Sprintf("showing first %d of %d matches", len(m.st.Rows), m.st.Count))
	}
	return ""
}

func (m model) helpText() string {
	switch {
	case m.busy && m.ctrl.ScanRunning():
		return "scanning… • esc: cancel"
	case m.focusTable:
		return "↑/↓ select • w/enter edit • f: freeze/unfreeze • tab: input • ctrl+z undo • f1: help"
	case m.mode == modeWrite:
		return "enter: write value • esc: cancel • tab: matches"
	case m.mode == modeRename:
		return "enter: name this tab • empty name clears it • esc: cancel"
	case m.lastScan != "":
		return "enter: scan • empty enter: repeat “" + m.lastScan + "” • tab: matches • ctrl+t: new tab • f1: help"
	default:
		return "enter: scan • tab: matches • ctrl+t: new tab • ctrl+d: close tab • f1: help • quit"
	}
}
