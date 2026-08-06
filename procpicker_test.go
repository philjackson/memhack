package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/phil/memhack/internal/memory"
	"github.com/phil/memhack/internal/scan"
)

// newPickerModel builds a picker-screen model preloaded with two processes.
func newPickerModel(t *testing.T) model {
	t.Helper()
	ctrl := &controller{jobs: make(chan job)}
	m := newModel(ctrl, scan.I32, nil, screenPicker, 0)
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = m2.(model)
	m3, _ := m.Update(procListMsg{procs: []memory.ProcInfo{
		{Pid: 111, Comm: "alpha"},
		{Pid: 222, Comm: "beta"},
	}})
	return m3.(model)
}

func TestPickerPopulatesFromProcListMsg(t *testing.T) {
	m := newPickerModel(t)
	if got := len(m.list.Items()); got != 2 {
		t.Fatalf("list has %d items, want 2", got)
	}
	if !strings.Contains(m.View(), "alpha") {
		t.Error("picker view should list process names")
	}
}

func TestPickerSelectIssuesAttach(t *testing.T) {
	m := newPickerModel(t)
	nm, cmd := m.Update(press("enter"))
	m = nm.(model)
	if !m.pendingAttach {
		t.Error("selecting a process should set pendingAttach")
	}
	if cmd == nil {
		t.Error("selecting should issue an attach command")
	}
	if !m.busy {
		t.Error("issuing an attach should mark the model busy")
	}
}

func TestPendingAttachSuccessSwitchesToScanner(t *testing.T) {
	m := newPickerModel(t)
	m.pendingAttach = true
	nm, _ := m.Update(stateMsg(state{Attached: true, Pid: 111, Type: scan.I32}))
	m = nm.(model)
	if m.screen != screenScanner {
		t.Error("a successful attach should switch to the scanner screen")
	}
	if m.pendingAttach {
		t.Error("pendingAttach should be cleared after the state arrives")
	}
}

func TestPendingAttachFailureStaysOnPicker(t *testing.T) {
	m := newPickerModel(t)
	m.pendingAttach = true
	nm, _ := m.Update(stateMsg(state{Type: scan.I32, Err: errNotAttached}))
	m = nm.(model)
	if m.screen != screenPicker {
		t.Error("a failed attach should stay on the picker")
	}
	if !strings.Contains(m.errMsg, "not attached") {
		t.Errorf("errMsg = %q, want the attach error", m.errMsg)
	}
}

func TestPickerQuitKeys(t *testing.T) {
	for _, k := range []string{"ctrl+c", "q"} {
		m := newPickerModel(t)
		_, cmd := m.Update(press(k))
		assertQuit(t, cmd)
	}

	// ctrl+d closes a tab elsewhere, so it must not quit from here either:
	// the picker can be reopened mid-session with tabs full of matches.
	m := newPickerModel(t)
	_, cmd := m.Update(press("ctrl+d"))
	if cmd != nil {
		if _, isQuit := cmd().(tea.QuitMsg); isQuit {
			t.Error("ctrl+d must not quit from the picker")
		}
	}
}

func TestPickerReloadKey(t *testing.T) {
	m := newPickerModel(t)
	_, cmd := m.Update(press("ctrl+r"))
	if cmd == nil {
		t.Fatal("ctrl+r should issue a reload command")
	}
	if _, ok := cmd().(procListMsg); !ok {
		t.Error("ctrl+r should reload the process list")
	}
}

func TestProcListReopenViaCommand(t *testing.T) {
	// From the scanner screen, ":ps" reopens the picker.
	m := newTestModel()
	m = typeString(m, ":ps")
	nm, cmd := m.Update(press("enter"))
	m = nm.(model)
	if m.screen != screenPicker {
		t.Error(":ps should switch to the picker screen")
	}
	if cmd == nil {
		t.Error(":ps should trigger a process-list load")
	}
}

func TestProcListScrollKeysBound(t *testing.T) {
	l := newProcList()
	has := func(b key.Binding, want string) bool {
		for _, k := range b.Keys() {
			if k == want {
				return true
			}
		}
		return false
	}
	if !has(l.KeyMap.CursorDown, "ctrl+j") {
		t.Error("ctrl+j should be bound to scroll down")
	}
	if !has(l.KeyMap.CursorUp, "ctrl+k") {
		t.Error("ctrl+k should be bound to scroll up")
	}
	// The "/" filter binding is a stock list feature; confirm it's present.
	if !has(l.KeyMap.Filter, "/") {
		t.Error("/ should open the filter")
	}
}

func TestProcItemFilterValue(t *testing.T) {
	it := procItem{memory.ProcInfo{Pid: 1234, Comm: "bash", Cmdline: "/bin/bash -i"}}
	fv := it.FilterValue()
	for _, want := range []string{"1234", "bash", "/bin/bash"} {
		if !strings.Contains(fv, want) {
			t.Errorf("FilterValue %q should contain %q", fv, want)
		}
	}
}
