package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/phil/memhack/internal/scan"
)

// maxTabs bounds how many searches can be open at once. Each tab keeps its own
// match set plus up to maxHistory undo snapshots of it, and a first scan can
// match millions of addresses, so the count is capped rather than open-ended.
const maxTabs = 9

// tab is one independent search: its own matches (with their undo history),
// data type, scan alignment, and last scan expression.
//
// Tabs share the attached process and the frozen-address map, because those
// belong to the target rather than to any one search: a value frozen while
// working in one tab must keep being held while you work in another.
type tab struct {
	name     string        // user-given name; empty until renamed
	sc       *scan.Scanner // nil until a process is attached
	dt       scan.DataType
	align    int
	lastScan string // last expression scanned here, for instant repeat
}

// label is what the tab is called in the interface: its name if it has one,
// else the scan that last ran in it, else a placeholder.
func (t *tab) label() string {
	switch {
	case t.name != "":
		return t.name
	case t.lastScan != "":
		return t.lastScan
	}
	return "empty"
}

// tabSet is the open searches and which of them is current. Both interfaces
// (the TUI's worker and the REPL's app) embed one, so tab bookkeeping behaves
// identically in each. There is always at least one tab.
type tabSet struct {
	tabs []*tab
	cur  int
}

// newTabSet starts a set holding a single empty tab.
func newTabSet(dt scan.DataType, align int) tabSet {
	if align < 0 {
		align = 0
	}
	return tabSet{tabs: []*tab{{dt: dt, align: align}}}
}

// active returns the tab currently being worked on.
func (s *tabSet) active() *tab { return s.tabs[s.cur] }

// open adds a tab and switches to it. It starts with no matches but inherits
// the current tab's type and alignment, which is nearly always what a second
// search of the same target wants. The caller gives it a scanner if a process
// is attached.
func (s *tabSet) open(name string) (*tab, error) {
	if len(s.tabs) >= maxTabs {
		return nil, fmt.Errorf("tab limit reached (%d open); close one first", maxTabs)
	}
	cur := s.active()
	t := &tab{name: strings.TrimSpace(name), dt: cur.dt, align: cur.align}
	s.tabs = append(s.tabs, t)
	s.cur = len(s.tabs) - 1
	return t, nil
}

// closeActive discards the current tab and returns it. Dropping the tab drops
// its matches and undo history with it, releasing that memory. The last tab is
// never closed: there is always somewhere to scan.
func (s *tabSet) closeActive() (*tab, error) {
	if len(s.tabs) == 1 {
		return nil, errors.New("the last tab can't be closed (reset it instead)")
	}
	closed := s.active()
	s.tabs = append(s.tabs[:s.cur], s.tabs[s.cur+1:]...)
	if s.cur >= len(s.tabs) {
		s.cur = len(s.tabs) - 1
	}
	return closed, nil
}

// selectAt switches to tab i (0-based).
func (s *tabSet) selectAt(i int) error {
	if i < 0 || i >= len(s.tabs) {
		return fmt.Errorf("no tab %d (open: 1-%d)", i+1, len(s.tabs))
	}
	s.cur = i
	return nil
}

// cycle moves delta tabs along, wrapping around at both ends.
func (s *tabSet) cycle(delta int) {
	n := len(s.tabs)
	s.cur = ((s.cur+delta)%n + n) % n
}

// rename names the current tab; an empty name clears it, falling back to the
// label derived from its last scan.
func (s *tabSet) rename(name string) { s.active().name = strings.TrimSpace(name) }

// summary is a one-line list of the open tabs for a status message, with the
// current one marked.
func (s *tabSet) summary() string {
	parts := make([]string, 0, len(s.tabs))
	for i, t := range s.tabs {
		mark := " "
		if i == s.cur {
			mark = "▸"
		}
		parts = append(parts, fmt.Sprintf("%s%d %s", mark, i+1, t.label()))
	}
	return "tabs: " + strings.Join(parts, " · ")
}

// tabAction is what a parsed "tab" command asks for.
type tabAction int

const (
	tabList   tabAction = iota // no arguments: report the open tabs
	tabNew                     // new [name]
	tabClose                   // close
	tabRename                  // rename <name>
	tabSelect                  // <n>
)

// tabCmd is a parsed "tab" command, shared by the TUI's ":tab" and the REPL's
// "tab" so both accept exactly the same forms.
type tabCmd struct {
	action tabAction
	name   string // for tabNew (may be empty) and tabRename
	index  int    // 0-based, for tabSelect
}

// tabUsage documents the command in errors and help output.
const tabUsage = "usage: tab [new [name] | close | rename <name> | <n>]"

// parseTabCmd interprets the arguments of a "tab" command.
func parseTabCmd(args []string) (tabCmd, error) {
	if len(args) == 0 {
		return tabCmd{action: tabList}, nil
	}
	switch args[0] {
	case "new", "n":
		return tabCmd{action: tabNew, name: strings.Join(args[1:], " ")}, nil
	case "close", "c":
		return tabCmd{action: tabClose}, nil
	case "rename", "name":
		name := strings.TrimSpace(strings.Join(args[1:], " "))
		if name == "" {
			return tabCmd{}, errors.New("usage: tab rename <name>")
		}
		return tabCmd{action: tabRename, name: name}, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return tabCmd{}, fmt.Errorf("unknown tab command %q (%s)", args[0], tabUsage)
	}
	return tabCmd{action: tabSelect, index: n - 1}, nil
}
