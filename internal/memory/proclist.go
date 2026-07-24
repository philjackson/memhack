package memory

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// ProcInfo describes a running process for display in a picker.
type ProcInfo struct {
	Pid     int
	Comm    string // short name from /proc/<pid>/comm
	Cmdline string // full command line, space-joined
	Uid     uint32 // owner of the process
}

// ListProcesses enumerates the processes currently visible in /proc. Entries
// that vanish mid-scan are skipped rather than reported as errors.
func ListProcesses() ([]ProcInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var procs []ProcInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid directory
		}
		base := "/proc/" + e.Name()

		p := ProcInfo{Pid: pid}
		if fi, err := os.Stat(base); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				p.Uid = st.Uid
			}
		}
		p.Comm = readTrimmed(base + "/comm")
		p.Cmdline = readCmdline(base + "/cmdline")
		if p.Comm == "" && p.Cmdline == "" {
			continue // exited between ReadDir and now
		}
		procs = append(procs, p)
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].Pid < procs[j].Pid })
	return procs, nil
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readCmdline reads a NUL-separated /proc/<pid>/cmdline into a space-joined
// string.
func readCmdline(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	return strings.Join(args, " ")
}
