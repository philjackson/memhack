package memory

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// These functions must all run on the same OS thread, because ptrace
// associates the tracer with a specific thread (task). The worker (TUI) and
// the REPL both lock their goroutine to one thread so every ptrace call and
// /proc/<pid>/mem access happens from that one tracer thread.
//
// The attach lifetime is per-operation: Attach seizes the target, stops it,
// and opens its memory; Detach closes the memory and releases the target so it
// runs freely again. Calls are reference-counted so nested Attach/Detach pairs
// behave. Crucially, the target is only ever ptrace-stopped between a matched
// Attach and Detach — never while memhack is idle.

// Attach stops the target and opens /proc/<pid>/mem for reading and writing.
// It seizes the process (required to access its memory under a non-permissive
// ptrace_scope) and interrupts it so the caller sees a consistent snapshot.
func (p *Process) Attach() error {
	if p.refs > 0 {
		p.refs++
		return nil
	}
	if err := unix.PtraceSeize(p.Pid); err != nil {
		return fmt.Errorf("ptrace seize %d: %w (need same-user + ptrace permission, or root)", p.Pid, err)
	}
	if err := unix.PtraceInterrupt(p.Pid); err != nil {
		unix.PtraceDetach(p.Pid)
		return fmt.Errorf("ptrace interrupt %d: %w", p.Pid, err)
	}
	var ws unix.WaitStatus
	if _, err := unix.Wait4(p.Pid, &ws, 0, nil); err != nil {
		unix.PtraceDetach(p.Pid)
		return fmt.Errorf("wait for %d to stop: %w", p.Pid, err)
	}
	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", p.Pid), os.O_RDWR, 0)
	if err != nil {
		unix.PtraceDetach(p.Pid)
		return fmt.Errorf("open mem for pid %d: %w", p.Pid, err)
	}
	p.f = f
	p.refs = 1
	return nil
}

// Detach releases the target once every matching Attach has been released,
// letting it run freely again (ptrace detach resumes a stopped tracee).
func (p *Process) Detach() error {
	if p.refs == 0 {
		return nil
	}
	p.refs--
	if p.refs > 0 {
		return nil
	}
	return p.teardown()
}

// forceDetach releases the attachment regardless of the reference count.
func (p *Process) forceDetach() {
	if p.refs == 0 {
		return
	}
	p.refs = 0
	p.teardown()
}

func (p *Process) teardown() error {
	if p.f != nil {
		p.f.Close()
		p.f = nil
	}
	if err := unix.PtraceDetach(p.Pid); err != nil {
		return fmt.Errorf("ptrace detach %d: %w", p.Pid, err)
	}
	return nil
}
