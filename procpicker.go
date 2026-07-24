package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/phil/memhack/internal/memory"
)

// procItem adapts a ProcInfo to the bubbles list item interface.
type procItem struct {
	memory.ProcInfo
}

func (p procItem) Title() string {
	name := p.Comm
	if name == "" {
		name = filepath.Base(strings.Fields(p.Cmdline + " ")[0])
	}
	return fmt.Sprintf("%-8d %s", p.Pid, name)
}

func (p procItem) Description() string {
	if p.Cmdline != "" {
		return p.Cmdline
	}
	return "(no command line)"
}

// FilterValue is what the "/" search matches against — pid, name, and args.
func (p procItem) FilterValue() string {
	return fmt.Sprintf("%d %s %s", p.Pid, p.Comm, p.Cmdline)
}

// procListMsg delivers the result of a process scan to the update loop.
type procListMsg struct {
	procs []memory.ProcInfo
	err   error
}

// loadProcs scans /proc for the current user's processes (all of them when
// running as root). It is a plain tea.Cmd: it touches no ptrace state, so it
// can run on any goroutine.
func loadProcs() tea.Msg {
	all, err := memory.ListProcesses()
	if err != nil {
		return procListMsg{err: err}
	}
	uid := os.Getuid()
	mine := make([]memory.ProcInfo, 0, len(all))
	for _, p := range all {
		if uid == 0 || int(p.Uid) == uid {
			mine = append(mine, p)
		}
	}
	return procListMsg{procs: mine}
}

// newProcList builds the list model used by the picker screen.
func newProcList() list.Model {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Select a process to attach to"
	l.SetStatusBarItemName("process", "processes")
	l.SetShowHelp(true)

	// "/" already opens the filter. Add ctrl+j / ctrl+k as scroll keys
	// alongside the defaults.
	l.KeyMap.CursorDown.SetKeys("down", "j", "ctrl+j")
	l.KeyMap.CursorUp.SetKeys("up", "k", "ctrl+k")
	l.KeyMap.CursorDown.SetHelp("↓/ctrl+j", "down")
	l.KeyMap.CursorUp.SetHelp("↑/ctrl+k", "up")

	// Surface a couple of extra bindings in the help line.
	l.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "attach")),
			key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "reload")),
		}
	}
	return l
}

// procItems converts scanned processes into list items.
func procItems(procs []memory.ProcInfo) []list.Item {
	items := make([]list.Item, len(procs))
	for i, p := range procs {
		items[i] = procItem{p}
	}
	return items
}

// setProcs loads the scanned processes into the picker list.
func (m *model) setProcs(procs []memory.ProcInfo) tea.Cmd {
	return m.list.SetItems(procItems(procs))
}
