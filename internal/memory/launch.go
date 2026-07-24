package memory

import (
	"fmt"
	"os"
	"os/exec"
)

// Launch starts a program as a child and returns a handle to it. The child
// runs untraced; operations attach to it on demand, the same as Open. Because
// the target is our own descendant, attaching works even under a restrictive
// yama ptrace_scope.
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
	return &Process{Pid: cmd.Process.Pid}, nil
}
