package memory

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// The functions here must all run on the same OS thread, because ptrace
// associates the tracer with a specific thread (task). main() locks the
// REPL goroutine to its thread so every ptrace call and /proc/mem access
// happens from that one tracer thread.

// seize attaches to the target with PTRACE_SEIZE, which — unlike
// PTRACE_ATTACH — leaves the process running. This lets scans observe a
// live, changing program; we only stop it momentarily around each access.
func (p *Process) seize() error {
	if err := unix.PtraceSeize(p.Pid); err != nil {
		return fmt.Errorf("ptrace seize %d: %w (need same-user + ptrace permission, or root)", p.Pid, err)
	}
	p.attached = true
	return nil
}

// Freeze stops the target and waits for it to actually halt, so a scan or
// write sees a consistent snapshot. Calls are reference-counted so nested
// Freeze/Thaw pairs behave.
func (p *Process) Freeze() error {
	if !p.attached {
		return nil
	}
	if p.frozen > 0 {
		p.frozen++
		return nil
	}
	if err := unix.PtraceInterrupt(p.Pid); err != nil {
		return fmt.Errorf("ptrace interrupt %d: %w", p.Pid, err)
	}
	var ws unix.WaitStatus
	if _, err := unix.Wait4(p.Pid, &ws, 0, nil); err != nil {
		return fmt.Errorf("wait for %d to stop: %w", p.Pid, err)
	}
	p.frozen = 1
	return nil
}

// Thaw resumes the target once every matching Freeze has been released.
func (p *Process) Thaw() error {
	if !p.attached || p.frozen == 0 {
		return nil
	}
	p.frozen--
	if p.frozen > 0 {
		return nil
	}
	if err := unix.PtraceCont(p.Pid, 0); err != nil {
		return fmt.Errorf("ptrace cont %d: %w", p.Pid, err)
	}
	return nil
}

// detach releases the target, leaving it running.
func (p *Process) detach() error {
	if !p.attached {
		return nil
	}
	p.attached = false
	p.frozen = 0
	if err := unix.PtraceDetach(p.Pid); err != nil {
		return fmt.Errorf("ptrace detach %d: %w", p.Pid, err)
	}
	return nil
}
