package memory

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Region describes a single mapped memory region of a process, as reported
// by /proc/<pid>/maps.
type Region struct {
	Start   uint64
	End     uint64
	Read    bool
	Write   bool
	Exec    bool
	Private bool
	Offset  uint64
	Path    string
}

// Size returns the length of the region in bytes.
func (r Region) Size() uint64 { return r.End - r.Start }

// String renders the region roughly the way /proc/<pid>/maps does.
func (r Region) String() string {
	perms := []byte("----")
	if r.Read {
		perms[0] = 'r'
	}
	if r.Write {
		perms[1] = 'w'
	}
	if r.Exec {
		perms[2] = 'x'
	}
	if r.Private {
		perms[3] = 'p'
	} else {
		perms[3] = 's'
	}
	return fmt.Sprintf("%012x-%012x %s %s", r.Start, r.End, string(perms), r.Path)
}

// ReadMaps parses /proc/<pid>/maps and returns all mapped regions.
func ReadMaps(pid int) ([]Region, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil, fmt.Errorf("open maps for pid %d: %w", pid, err)
	}
	defer f.Close()

	var regions []Region
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		r, ok := parseMapLine(sc.Text())
		if ok {
			regions = append(regions, r)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read maps for pid %d: %w", pid, err)
	}
	return regions, nil
}

// parseMapLine parses one line of /proc/<pid>/maps. Format:
//
//	start-end perms offset dev inode  pathname
func parseMapLine(line string) (Region, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return Region{}, false
	}

	addrs := strings.SplitN(fields[0], "-", 2)
	if len(addrs) != 2 {
		return Region{}, false
	}
	start, err := strconv.ParseUint(addrs[0], 16, 64)
	if err != nil {
		return Region{}, false
	}
	end, err := strconv.ParseUint(addrs[1], 16, 64)
	if err != nil {
		return Region{}, false
	}

	perms := fields[1]
	if len(perms) < 4 {
		return Region{}, false
	}
	offset, _ := strconv.ParseUint(fields[2], 16, 64)

	r := Region{
		Start:   start,
		End:     end,
		Read:    perms[0] == 'r',
		Write:   perms[1] == 'w',
		Exec:    perms[2] == 'x',
		Private: perms[3] == 'p',
		Offset:  offset,
	}
	if len(fields) >= 6 {
		r.Path = strings.Join(fields[5:], " ")
	}
	return r, true
}

// Scannable reports whether a region is a sensible target for value scanning:
// readable, writable, and not a special pseudo-mapping.
func (r Region) Scannable() bool {
	if !r.Read || !r.Write {
		return false
	}
	switch r.Path {
	case "[vvar]", "[vdso]", "[vsyscall]":
		return false
	}
	return true
}
