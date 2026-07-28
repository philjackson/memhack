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

// compileTarget compiles the given C program and returns the binary path,
// skipping the test if no C compiler is available.
func compileTarget(t *testing.T, program string) string {
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
	if err := os.WriteFile(src, []byte(program), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if out, err := exec.Command(cc, "-O0", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("compile target: %v\n%s", err, out)
	}
	return bin
}

// buildTarget compiles the standard test target that holds a known int and
// string in globals and then idles.
func buildTarget(t *testing.T) string {
	return compileTarget(t, `
#include <unistd.h>
volatile int magic = 1337;
volatile char tag[16] = "PLAYER_ONE";
int main(void) {
    for (;;) { sleep(1); (void)magic; (void)tag[0]; }
    return 0;
}
`)
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

func TestDetachedBetweenOperations(t *testing.T) {
	// The core of the idle-freeze fix: the target must never be left attached
	// (ptrace-stopped) between operations.
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

	if proc.Attached() {
		t.Error("process should not be attached immediately after launch")
	}

	s := NewScanner(proc, I32)
	if err := s.Scan(Cond{Op: Equal, Value: 1337}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if proc.Attached() {
		t.Error("process should be detached after a scan completes")
	}

	s.Refresh()
	if proc.Attached() {
		t.Error("process should be detached after a refresh")
	}

	if len(s.Matches) > 0 {
		raw, _ := I32.Encode("42")
		if err := s.Write(s.Matches[0], raw); err != nil {
			t.Fatalf("write: %v", err)
		}
		if proc.Attached() {
			t.Error("process should be detached after a write")
		}
	}
}

func TestStringAndBytesScan(t *testing.T) {
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

	// String scan for the known global.
	s := NewScanner(proc, String)
	if err := s.Scan(Cond{Op: Equal, Bytes: []byte("PLAYER_ONE")}); err != nil {
		t.Fatalf("string scan: %v", err)
	}
	if len(s.Matches) == 0 {
		t.Fatal("expected to find the PLAYER_ONE string")
	}
	if got := String.Format(s.Matches[0].Last); got != "PLAYER_ONE" {
		t.Errorf("match value = %q, want PLAYER_ONE", got)
	}
	if proc.Attached() {
		t.Error("process should be detached after a string scan")
	}

	// Byte-array scan for the same bytes (hex of "PLAYER_ONE" starts 50 4c 41).
	b := NewScanner(proc, Bytes)
	if err := b.Scan(Cond{Op: Equal, Bytes: []byte{0x50, 0x4c, 0x41, 0x59}}); err != nil { // "PLAY"
		t.Fatalf("bytes scan: %v", err)
	}
	if len(b.Matches) == 0 {
		t.Fatal("expected to find the PLAY byte pattern")
	}
	if got := Bytes.Format(b.Matches[0].Last); got != "50 4c 41 59" {
		t.Errorf("match value = %q, want \"50 4c 41 59\"", got)
	}

	// Narrowing a string scan with 'unchanged' keeps the (static) match.
	before := len(s.Matches)
	if err := s.Scan(Cond{Op: Unchanged}); err != nil {
		t.Fatalf("unchanged narrow: %v", err)
	}
	if len(s.Matches) != before {
		t.Errorf("unchanged narrow changed the count: %d -> %d", before, len(s.Matches))
	}
}

func TestAlignmentSkipsUnaligned(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// A distinctive value (0x11223344, little-endian 44 33 22 11) placed at an
	// unaligned offset within a 16-aligned global, so blob+1 is never a
	// multiple of 4. Static init means it's present at load, before main runs.
	const val = 287454020 // 0x11223344
	bin := compileTarget(t, `
#include <unistd.h>
volatile unsigned char blob[32] __attribute__((aligned(16))) =
    {0, 0x44, 0x33, 0x22, 0x11};
int main(void) {
    for (;;) { sleep(1); (void)blob[0]; }
    return 0;
}
`)
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

	// Byte-granular scan finds the unaligned value.
	byteScan := NewScanner(proc, I32)
	byteScan.Align = 1
	if err := byteScan.Scan(Cond{Op: Equal, Value: val}); err != nil {
		t.Fatalf("byte scan: %v", err)
	}
	c1 := len(byteScan.Matches)

	// Type-width alignment (the fast default) steps by 4 and skips it.
	alignedScan := NewScanner(proc, I32) // Align 0 -> type width (4)
	if alignedScan.Alignment() != 4 {
		t.Fatalf("default alignment = %d, want 4", alignedScan.Alignment())
	}
	if err := alignedScan.Scan(Cond{Op: Equal, Value: val}); err != nil {
		t.Fatalf("aligned scan: %v", err)
	}
	c2 := len(alignedScan.Matches)

	if c1 < 1 {
		t.Fatal("byte-granular scan should find the unaligned value")
	}
	if c1 <= c2 {
		t.Errorf("byte scan found %d, aligned scan found %d; alignment should skip the unaligned value", c1, c2)
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

func TestScanReportsProgress(t *testing.T) {
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
	var got []Progress
	s.Progress = func(p Progress) { got = append(got, p) }

	// The initial sweep is measured in bytes of scannable memory.
	if err := s.Scan(Cond{Op: Equal, Value: 1337}); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	checkProgress(t, got, UnitBytes)

	// Narrowing is measured in matches re-checked.
	got = nil
	if err := s.Scan(Cond{Op: Equal, Value: 1337}); err != nil {
		t.Fatalf("narrow scan: %v", err)
	}
	checkProgress(t, got, UnitMatches)
}

// checkProgress asserts that a scan's updates are in the expected unit, never
// go backwards or past the total, and finish at 100%.
func checkProgress(t *testing.T, got []Progress, unit string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("scan reported no progress at all")
	}
	var prev uint64
	for i, p := range got {
		if p.Unit != unit {
			t.Errorf("update %d: unit = %q, want %q", i, p.Unit, unit)
		}
		if p.Total == 0 {
			t.Errorf("update %d: total is 0, so the bar would have nothing to scale to", i)
		}
		if p.Done < prev {
			t.Errorf("update %d: progress went backwards: %d after %d", i, p.Done, prev)
		}
		if p.Done > p.Total {
			t.Errorf("update %d: done %d exceeds total %d", i, p.Done, p.Total)
		}
		if f := p.Fraction(); f < 0 || f > 1 {
			t.Errorf("update %d: fraction %v out of range", i, f)
		}
		prev = p.Done
	}
	last := got[len(got)-1]
	if last.Done != last.Total || last.Fraction() != 1 {
		t.Errorf("a completed scan should end at 100%%: got %d of %d", last.Done, last.Total)
	}
}
