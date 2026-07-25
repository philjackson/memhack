package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phil/memhack/internal/scan"
)

// buildCTarget compiles a tiny C program holding a known value, skipping if no
// C compiler is available.
func buildCTarget(t *testing.T) string {
	t.Helper()
	var cc string
	for _, c := range []string{"cc", "gcc", "clang"} {
		if p, err := exec.LookPath(c); err == nil {
			cc = p
			break
		}
	}
	if cc == "" {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "t.c")
	bin := filepath.Join(dir, "t")
	const prog = `
#include <unistd.h>
volatile int magic = 1337;
int main(void){ for(;;){ sleep(1); (void)magic; } return 0; }
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if out, err := exec.Command(cc, "-O0", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("compile: %v\n%s", err, out)
	}
	return bin
}

// run executes a controller command synchronously and returns the resulting
// state. The returned tea.Cmd just does a channel round-trip to the worker,
// so calling it from the test goroutine is safe.
func run(t *testing.T, cmd tea.Cmd) state {
	t.Helper()
	msg := cmd()
	st, ok := msg.(stateMsg)
	if !ok {
		t.Fatalf("expected stateMsg, got %T", msg)
	}
	return state(st)
}

func TestControllerLaunchScanWriteUndo(t *testing.T) {
	bin := buildCTarget(t)
	ctrl := newController(scan.I32, 0)

	st := run(t, ctrl.launch([]string{bin}))
	if st.Err != nil {
		if strings.Contains(st.Err.Error(), "not permitted") || strings.Contains(st.Err.Error(), "denied") {
			t.Skipf("ptrace not permitted here: %v", st.Err)
		}
		t.Fatalf("launch: %v", st.Err)
	}
	pid := st.Pid
	if !st.Attached || pid == 0 {
		t.Fatal("expected attached with a pid")
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	})

	// First scan for the known value.
	st = run(t, ctrl.scanExpr("1337"))
	if st.Err != nil {
		t.Fatalf("scan: %v", st.Err)
	}
	if st.Count == 0 {
		t.Fatal("expected at least one match for 1337")
	}
	firstCount := st.Count

	// A row should be present and show the value.
	if len(st.Rows) == 0 {
		t.Fatal("expected display rows")
	}
	if st.Rows[0].Value != "1337" {
		t.Errorf("row value = %q, want 1337", st.Rows[0].Value)
	}

	// Write a new value to match #0 and confirm the refreshed row reflects it.
	st = run(t, ctrl.write(0, "4242"))
	if st.Err != nil {
		t.Fatalf("write: %v", st.Err)
	}
	if st.Rows[0].Value != "4242" {
		t.Errorf("after write, row value = %q, want 4242", st.Rows[0].Value)
	}

	// Narrow to the new value; the match must survive and the set not grow.
	st = run(t, ctrl.scanExpr("4242"))
	if st.Err != nil {
		t.Fatalf("narrow: %v", st.Err)
	}
	if st.Count == 0 || st.Count > firstCount {
		t.Errorf("narrow count = %d, want in (0, %d]", st.Count, firstCount)
	}

	// Undo returns to the first-scan result.
	if !st.CanUndo {
		t.Fatal("expected CanUndo after two scans")
	}
	st = run(t, ctrl.undo())
	if st.Err != nil {
		t.Fatalf("undo: %v", st.Err)
	}
	if st.Count != firstCount {
		t.Errorf("after undo count = %d, want %d", st.Count, firstCount)
	}
}

func TestFreezeRewritesValue(t *testing.T) {
	// Drive the worker synchronously (no goroutine/ticker) so applyFreezes can
	// be invoked deterministically.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	bin := buildCTarget(t)
	w := &worker{dt: scan.I32, frozen: map[uint64][]byte{}, freezeEvery: time.Hour}
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
		t.Fatalf("scan: %v", err)
	}
	if len(w.sc.Matches) == 0 {
		t.Fatal("expected a match for 1337")
	}

	// Freeze match 0 at its current value (1337).
	if _, err := w.freezeIndex(0); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if len(w.frozen) != 1 {
		t.Fatalf("expected 1 frozen address, got %d", len(w.frozen))
	}

	// Overwrite the value; confirm the write took.
	if _, err := w.write(0, "9999"); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.sc.RefreshN(1)
	if got := scan.I32.Format(w.sc.Matches[0].Last); got != "9999" {
		t.Fatalf("write didn't take: value = %s", got)
	}

	// A freeze pass must restore the frozen value.
	w.applyFreezes()
	w.sc.RefreshN(1)
	if got := scan.I32.Format(w.sc.Matches[0].Last); got != "1337" {
		t.Errorf("freeze did not restore the value: got %s, want 1337", got)
	}

	// Unfreezing stops it from being restored.
	if _, err := w.freezeIndex(0); err != nil { // toggle off
		t.Fatalf("unfreeze: %v", err)
	}
	if len(w.frozen) != 0 {
		t.Errorf("toggling freeze off should leave 0 frozen, got %d", len(w.frozen))
	}
}

func TestControllerFreezeState(t *testing.T) {
	bin := buildCTarget(t)
	ctrl := newController(scan.I32, time.Hour) // slow freeze; no interference
	st := run(t, ctrl.launch([]string{bin}))
	if st.Err != nil {
		if strings.Contains(st.Err.Error(), "not permitted") || strings.Contains(st.Err.Error(), "denied") {
			t.Skipf("ptrace not permitted here: %v", st.Err)
		}
		t.Fatalf("launch: %v", st.Err)
	}
	pid := st.Pid
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	})

	st = run(t, ctrl.scanExpr("1337"))
	if st.Count == 0 {
		t.Fatal("expected a match")
	}
	st = run(t, ctrl.freeze(0))
	if st.Frozen != 1 {
		t.Errorf("Frozen = %d, want 1", st.Frozen)
	}
	if len(st.Rows) > 0 && !st.Rows[0].Frozen {
		t.Error("row 0 should be marked frozen")
	}
	st = run(t, ctrl.freeze(0)) // toggle off
	if st.Frozen != 0 {
		t.Errorf("Frozen = %d after toggle, want 0", st.Frozen)
	}
}

func TestControllerScanBeforeAttach(t *testing.T) {
	ctrl := newController(scan.I32, 0)
	st := run(t, ctrl.scanExpr("1"))
	if st.Err == nil {
		t.Fatal("expected an error scanning before attaching")
	}
}

func TestControllerSetType(t *testing.T) {
	ctrl := newController(scan.I32, 0)
	st := run(t, ctrl.setType("f32"))
	if st.Err != nil {
		t.Fatalf("setType: %v", st.Err)
	}
	if st.Type != scan.F32 {
		t.Errorf("type = %s, want f32", st.Type)
	}
	st = run(t, ctrl.setType("bogus"))
	if st.Err == nil {
		t.Error("expected error for bogus type")
	}
}
