package scan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/phil/memhack/internal/memory"
)

// buildTarget compiles a tiny C program that holds a known value in a global
// and then idles, returning the path to the binary. It skips the test if no C
// compiler is available.
func buildTarget(t *testing.T) string {
	t.Helper()
	var cc string
	for _, c := range []string{"cc", "gcc", "clang"} {
		if p, err := exec.LookPath(c); err == nil {
			cc = p
			break
		}
	}
	if cc == "" {
		t.Skip("no C compiler available; skipping ptrace integration test")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "target.c")
	bin := filepath.Join(dir, "target")
	const program = `
#include <unistd.h>
volatile int magic = 1337;
int main(void) {
    for (;;) { sleep(1); (void)magic; }
    return 0;
}
`
	if err := os.WriteFile(src, []byte(program), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	out, err := exec.Command(cc, "-O0", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("compile target: %v\n%s", err, out)
	}
	return bin
}

func TestScanWriteRoundTrip(t *testing.T) {
	// ptrace binds the tracer to one OS thread; keep every memory call here.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	bin := buildTarget(t)

	proc, err := memory.Launch(bin)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") ||
			strings.Contains(err.Error(), "permission denied") {
			t.Skipf("ptrace not permitted in this environment: %v", err)
		}
		t.Fatalf("launch: %v", err)
	}
	defer func() {
		proc.Close()
		if p, err := os.FindProcess(proc.Pid); err == nil {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	}()

	s := NewScanner(proc, I32)

	// First scan: the global initialised to 1337 must be found.
	if err := s.Scan(Cond{Op: Equal, Value: 1337}); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if len(s.Matches) == 0 {
		t.Fatal("expected at least one match for 1337")
	}
	firstCount := len(s.Matches)
	target := s.Matches[0]
	t.Logf("first scan: %d matches, using %#x", firstCount, target.Addr)

	// Write a new value and confirm the scanner reads it back.
	raw, err := I32.Encode("4242")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := s.Write(target, raw); err != nil {
		t.Fatalf("write: %v", err)
	}

	s.Refresh()
	var wrote *Match
	for i := range s.Matches {
		if s.Matches[i].Addr == target.Addr {
			wrote = &s.Matches[i]
			break
		}
	}
	if wrote == nil {
		t.Fatal("written address vanished from match set")
	}
	if got := I32.Format(wrote.Last); got != "4242" {
		t.Fatalf("after write, value at %#x = %s, want 4242", target.Addr, got)
	}

	// Narrowing to ==4242 must retain the address we wrote and never grow.
	if err := s.Scan(Cond{Op: Equal, Value: 4242}); err != nil {
		t.Fatalf("narrow scan: %v", err)
	}
	if len(s.Matches) > firstCount {
		t.Errorf("narrow grew match set: %d > %d", len(s.Matches), firstCount)
	}
	found := false
	for _, m := range s.Matches {
		if m.Addr == target.Addr {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("address %#x not retained after narrowing to its new value", target.Addr)
	}

	// Undo the narrowing scan: the match set must return to the first scan's
	// result (firstCount entries).
	if !s.CanUndo() {
		t.Fatal("expected an undo step to be available after scanning")
	}
	if !s.Undo() {
		t.Fatal("undo returned false")
	}
	if len(s.Matches) != firstCount {
		t.Errorf("after undo: %d matches, want %d (the first-scan result)", len(s.Matches), firstCount)
	}
}

func TestScanCancel(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	bin := buildTarget(t)
	proc, err := memory.Launch(bin)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "permission denied") {
			t.Skipf("ptrace not permitted here: %v", err)
		}
		t.Fatalf("launch: %v", err)
	}
	defer func() {
		proc.Close()
		if p, err := os.FindProcess(proc.Pid); err == nil {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	}()

	s := NewScanner(proc, I32)

	// A context cancelled before the scan starts must abort it immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = s.ScanContext(ctx, Cond{Op: Equal, Value: 1337})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("ScanContext = %v, want ErrCancelled", err)
	}
	// A cancelled scan must leave the scanner exactly as it was.
	if s.Scanned() {
		t.Error("cancelled first scan should leave the scanner unscanned")
	}
	if len(s.Matches) != 0 {
		t.Errorf("cancelled scan left %d matches, want 0", len(s.Matches))
	}
	if s.CanUndo() {
		t.Error("cancelled scan should not add an undo step")
	}

	// A normal scan still works afterward.
	if err := s.Scan(Cond{Op: Equal, Value: 1337}); err != nil {
		t.Fatalf("scan after cancel: %v", err)
	}
	if len(s.Matches) == 0 {
		t.Error("expected matches from the completed scan")
	}
}

func TestRelativeScanNeedsPrior(t *testing.T) {
	// A relative scan as the very first scan is an error; verify without
	// needing a live process by driving a scanner whose proc is never used.
	s := &Scanner{Type: I32}
	err := s.Scan(Cond{Op: Increased})
	if err == nil {
		t.Fatal("expected error for relative first scan")
	}
	if !strings.Contains(err.Error(), "prior scan") {
		t.Errorf("unexpected error: %v", err)
	}
}
