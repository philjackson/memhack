package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	ctrl := newController(scan.I32)

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

func TestControllerScanBeforeAttach(t *testing.T) {
	ctrl := newController(scan.I32)
	st := run(t, ctrl.scanExpr("1"))
	if st.Err == nil {
		t.Fatal("expected an error scanning before attaching")
	}
}

func TestControllerSetType(t *testing.T) {
	ctrl := newController(scan.I32)
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
