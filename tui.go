package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/phil/memhack/internal/scan"
)

const (
	// defaultWatchInterval is how often the live watch re-reads values. Each
	// tick attaches, reads, and detaches, so the target is only briefly
	// stopped per tick and runs freely in between; a modest interval keeps it
	// unintrusive (especially for large, multithreaded targets).
	defaultWatchInterval = 1 * time.Second
	scanPrompt           = "scan› "
)

// inputMode selects how the text input's contents are interpreted on Enter.
type inputMode int

const (
	modeScan  inputMode = iota // a scan expression (or ":" command)
	modeWrite                  // a value to write to the selected match
)

// tickMsg drives the periodic refresh of displayed values.
type tickMsg time.Time

// screen selects which view is active.
type screen int

const (
	screenScanner screen = iota // the matches table + scan input
	screenPicker                // the process picker
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("57")).Padding(0, 1)
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

type model struct {
	ctrl *controller

	input textinput.Model
	table table.Model
	spin  spinner.Model
	list  list.Model

	screen        screen
	pendingAttach bool

	st         state
	mode       inputMode
	writeIdx   int
	focusTable bool
	busy       bool
	status     string
	errMsg     string
	lastScan   string   // last scan expression, for instant repeat
	history    []string // submitted scan expressions and commands, oldest first
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
	ti.Placeholder = "e.g. 1337, > 100, 10..20, inc, :type f32"
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

	return model{
		ctrl:          ctrl,
		input:         ti,
		table:         tbl,
		spin:          sp,
		list:          newProcList(),
		screen:        start,
		mode:          modeScan,
		st:            state{Type: dt},
		startup:       startup,
		watchInterval: watch,
	}
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, tickCmd(m.watchInterval)}
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
			cmds = append(cmds, m.markBusy(), m.ctrl.refresh())
		}
		return m, tea.Batch(cmds...)

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
		m.busy = false
		m.st = state(msg)
		m.refreshTable()
		return m, nil

	case stateMsg:
		m.busy = false
		m.st = state(msg)
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
		if m.screen == screenPicker {
			return m.handlePickerKey(msg)
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

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m, tea.Quit
	case "ctrl+z":
		return m.issue(m.ctrl.undo())
	case "ctrl+r":
		return m.issue(m.ctrl.reset())
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
		if m.mode == modeWrite {
			m.cancelWrite()
			return m, nil
		}
		// Cancel an in-progress scan.
		if m.busy && m.ctrl.ScanRunning() {
			m.ctrl.CancelScan()
			m.status = "cancelling scan…"
		}
		return m, nil
	}

	if m.focusTable {
		return m.handleTableKey(msg)
	}
	return m.handleInputKey(msg)
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
			m.cancelWrite() // reset prompt/mode; the value is captured below
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
	case "ctrl+c", "ctrl+d", "q":
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
	case "ps", "procs":
		return m.openPicker()
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

// issue marks the model busy and runs a worker command.
func (m model) issue(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if cmd == nil {
		return m, nil
	}
	return m, tea.Batch(cmd, m.markBusy())
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
func (m *model) markBusy() tea.Cmd {
	if m.busy {
		return nil
	}
	m.busy = true
	return m.spin.Tick
}

func (m *model) toggleFocus() {
	m.focusTable = !m.focusTable
	if m.focusTable {
		m.input.Blur()
		m.table.Focus()
	} else {
		m.table.Blur()
		if m.mode == modeWrite {
			m.resetInputToScan()
		}
		m.input.Focus()
	}
}

func (m *model) cancelWrite() {
	m.resetInputToScan()
	m.status = "write cancelled"
}

func (m *model) resetInputToScan() {
	m.mode = modeScan
	m.input.Reset()
	m.input.Prompt = scanPrompt
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

	// Reserve lines for title, status, input, message, and help.
	h := m.height - 9
	if h < 3 {
		h = 3
	}
	m.table.SetHeight(h)
}

func (m model) View() string {
	if m.screen == screenPicker {
		return m.pickerView()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("memhack") + "  " + m.statusLine() + "\n\n")
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
		return "↑/↓ select • w/enter edit • f: freeze/unfreeze • tab: input • ctrl+z undo • ctrl+r reset • ctrl+c quit"
	case m.mode == modeWrite:
		return "enter: write value • esc: cancel • tab: matches"
	case m.lastScan != "":
		return "enter: scan • empty enter: repeat “" + m.lastScan + "” • ↑/↓ history • ctrl+p: pause watch • tab: matches • quit"
	default:
		return "enter: scan • :type :set :align • ↑/↓ history • ctrl+p: pause watch • tab: matches • ctrl+z undo • quit/ctrl+c/ctrl+d"
	}
}
