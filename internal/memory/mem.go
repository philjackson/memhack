package memory

import (
	"fmt"
	"os"
)

// Process represents an attached target process whose memory we can read
// and write through /proc/<pid>/mem.
type Process struct {
	Pid      int
	f        *os.File
	attached bool
	frozen   int
}

// Open attaches to the given pid: it ptrace-seizes the target (which the
// kernel requires before another process may read /proc/<pid>/mem under a
// non-permissive ptrace_scope) and opens that file for reading and writing.
//
// It must be called from an OS-thread-locked goroutine, and every later
// method on the returned Process must run on that same thread, because ptrace
// binds the tracer to one thread.
func Open(pid int) (*Process, error) {
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return nil, fmt.Errorf("no such process %d: %w", pid, err)
	}

	p := &Process{Pid: pid}
	if err := p.seize(); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", pid), os.O_RDWR, 0)
	if err != nil {
		p.detach()
		return nil, fmt.Errorf("open mem for pid %d: %w", pid, err)
	}
	p.f = f
	return p, nil
}

// Close detaches from the target (leaving it running) and releases the
// /proc/<pid>/mem handle.
func (p *Process) Close() error {
	p.detach()
	if p.f == nil {
		return nil
	}
	return p.f.Close()
}

// ReadAt reads len(buf) bytes from the process's virtual address addr.
// It uses pread semantics so concurrent scans do not disturb a shared offset.
func (p *Process) ReadAt(buf []byte, addr uint64) (int, error) {
	n, err := p.f.ReadAt(buf, int64(addr))
	if err != nil && n == 0 {
		return n, fmt.Errorf("read %d bytes at %#x: %w", len(buf), addr, err)
	}
	return n, nil
}

// WriteAt writes buf to the process's virtual address addr.
func (p *Process) WriteAt(buf []byte, addr uint64) (int, error) {
	n, err := p.f.WriteAt(buf, int64(addr))
	if err != nil {
		return n, fmt.Errorf("write %d bytes at %#x: %w", len(buf), addr, err)
	}
	return n, nil
}

// Alive reports whether the target process still exists.
func (p *Process) Alive() bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", p.Pid))
	return err == nil
}
