package memory

import (
	"fmt"
	"os"
	"os/exec"
)

// Launch starts a program as a child of this process and attaches to it.
// Because the target is our own descendant, this works even under a
// restrictive yama ptrace_scope, where attaching to an unrelated same-user
// process would be denied.
//
// Like Open, it must run on the OS-thread-locked tracer goroutine.
func Launch(path string, args ...string) (*Process, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch %s: %w", path, err)
	}
	pid := cmd.Process.Pid

	p := &Process{Pid: pid}
	if err := p.seize(); err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", pid), os.O_RDWR, 0)
	if err != nil {
		p.detach()
		cmd.Process.Kill()
		return nil, fmt.Errorf("open mem for pid %d: %w", pid, err)
	}
	p.f = f
	return p, nil
}
