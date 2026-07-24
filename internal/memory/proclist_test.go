package memory

import (
	"os"
	"testing"
)

func TestListProcessesIncludesSelf(t *testing.T) {
	procs, err := ListProcesses()
	if err != nil {
		t.Fatalf("ListProcesses: %v", err)
	}
	if len(procs) == 0 {
		t.Fatal("expected at least one process")
	}

	self := os.Getpid()
	var me *ProcInfo
	for i := range procs {
		if procs[i].Pid == self {
			me = &procs[i]
			break
		}
	}
	if me == nil {
		t.Fatalf("current process %d not found in list", self)
	}
	if me.Comm == "" {
		t.Error("expected a non-empty comm for the current process")
	}
	if int(me.Uid) != os.Getuid() {
		t.Errorf("self uid = %d, want %d", me.Uid, os.Getuid())
	}
}

func TestReadCmdlineJoinsArgs(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cmdline"
	if err := os.WriteFile(path, []byte("bash\x00-i\x00-c\x00echo hi\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := readCmdline(path), "bash -i -c echo hi"; got != want {
		t.Errorf("readCmdline = %q, want %q", got, want)
	}
	// Empty file yields an empty string, not an error.
	empty := dir + "/empty"
	os.WriteFile(empty, nil, 0o644)
	if got := readCmdline(empty); got != "" {
		t.Errorf("readCmdline(empty) = %q, want empty", got)
	}
}
