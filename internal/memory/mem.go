package memory

import (
	"fmt"
	"os"
)

// Process is a handle to a target process. It does NOT hold a ptrace
// attachment or an open /proc/<pid>/mem descriptor while idle: those are
// acquired only for the duration of an operation, via Attach/Detach. Between
// operations the target runs completely untraced, so it is never left parked
// in a ptrace stop.
//
// All methods must run on one OS-thread-locked goroutine, because ptrace binds
// the tracer to the thread that attached.
type Process struct {
	Pid  int
	f    *os.File // open only while attached
	refs int      // Attach/Detach nesting depth; >0 means currently attached
}

// Open returns a handle to an existing process. It validates that the process
// exists but does not attach; the first operation attaches on demand.
func Open(pid int) (*Process, error) {
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return nil, fmt.Errorf("no such process %d: %w", pid, err)
	}
	return &Process{Pid: pid}, nil
}

// Close releases any attachment still held.
func (p *Process) Close() error {
	p.forceDetach()
	return nil
}

// Attached reports whether the process is currently attached.
func (p *Process) Attached() bool { return p.refs > 0 }

// ReadAt reads len(buf) bytes from the process's virtual address addr. The
// process must be attached (see Attach). It uses pread semantics so scans do
// not disturb a shared offset.
func (p *Process) ReadAt(buf []byte, addr uint64) (int, error) {
	if p.f == nil {
		return 0, fmt.Errorf("read at %#x: not attached", addr)
	}
	n, err := p.f.ReadAt(buf, int64(addr))
	if err != nil && n == 0 {
		return n, fmt.Errorf("read %d bytes at %#x: %w", len(buf), addr, err)
	}
	return n, nil
}

// WriteAt writes buf to the process's virtual address addr. The process must
// be attached.
func (p *Process) WriteAt(buf []byte, addr uint64) (int, error) {
	if p.f == nil {
		return 0, fmt.Errorf("write at %#x: not attached", addr)
	}
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
