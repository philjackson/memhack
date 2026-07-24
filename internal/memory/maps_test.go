package memory

import (
	"os"
	"testing"
)

func TestParseMapLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Region
		ok   bool
	}{
		{
			name: "heap rw private",
			line: "5612578bd000-5612578de000 rw-p 00000000 00:00 0                          [heap]",
			want: Region{Start: 0x5612578bd000, End: 0x5612578de000, Read: true, Write: true, Exec: false, Private: true, Offset: 0, Path: "[heap]"},
			ok:   true,
		},
		{
			name: "code r-x with file offset",
			line: "55c8e8e00000-55c8e8e21000 r-xp 00002000 08:01 1234567 /usr/bin/target",
			want: Region{Start: 0x55c8e8e00000, End: 0x55c8e8e21000, Read: true, Write: false, Exec: true, Private: true, Offset: 0x2000, Path: "/usr/bin/target"},
			ok:   true,
		},
		{
			name: "shared mapping",
			line: "7f0000000000-7f0000001000 rw-s 00000000 00:00 0 /dev/shm/thing",
			want: Region{Start: 0x7f0000000000, End: 0x7f0000001000, Read: true, Write: true, Private: false, Path: "/dev/shm/thing"},
			ok:   true,
		},
		{
			name: "anonymous no path",
			line: "7ffff7a00000-7ffff7a21000 rw-p 00000000 00:00 0",
			want: Region{Start: 0x7ffff7a00000, End: 0x7ffff7a21000, Read: true, Write: true, Private: true},
			ok:   true,
		},
		{
			name: "path with spaces",
			line: "400000-401000 r--p 00000000 08:01 42 /tmp/my file (deleted)",
			want: Region{Start: 0x400000, End: 0x401000, Read: true, Private: true, Path: "/tmp/my file (deleted)"},
			ok:   true,
		},
		{name: "blank", line: "", ok: false},
		{name: "garbage", line: "not a maps line", ok: false},
		{name: "bad address range", line: "zzzz rw-p 0 0:0 0", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseMapLine(tt.line)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if got != tt.want {
				t.Errorf("parseMapLine mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestRegionSize(t *testing.T) {
	r := Region{Start: 0x1000, End: 0x5000}
	if got := r.Size(); got != 0x4000 {
		t.Errorf("Size() = %#x, want %#x", got, 0x4000)
	}
}

func TestRegionScannable(t *testing.T) {
	tests := []struct {
		name string
		r    Region
		want bool
	}{
		{"rw anon", Region{Read: true, Write: true}, true},
		{"rw heap", Region{Read: true, Write: true, Path: "[heap]"}, true},
		{"read only", Region{Read: true, Write: false}, false},
		{"write only", Region{Read: false, Write: true}, false},
		{"rw vvar excluded", Region{Read: true, Write: true, Path: "[vvar]"}, false},
		{"rw vdso excluded", Region{Read: true, Write: true, Path: "[vdso]"}, false},
		{"rw vsyscall excluded", Region{Read: true, Write: true, Path: "[vsyscall]"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.Scannable(); got != tt.want {
				t.Errorf("Scannable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegionString(t *testing.T) {
	r := Region{Start: 0x1000, End: 0x2000, Read: true, Write: true, Private: true, Path: "[heap]"}
	got := r.String()
	want := "000000001000-000000002000 rw-p [heap]"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	shared := Region{Start: 0x10, End: 0x20, Read: true, Exec: true, Private: false}
	if got := shared.String(); got != "000000000010-000000000020 r-xs " {
		t.Errorf("shared String() = %q", got)
	}
}

func TestReadMapsSelf(t *testing.T) {
	// Reading our own maps needs no privileges and must always yield some
	// scannable region (the heap / anonymous mappings, at least).
	regions, err := ReadMaps(os.Getpid())
	if err != nil {
		t.Fatalf("ReadMaps(self): %v", err)
	}
	if len(regions) == 0 {
		t.Fatal("ReadMaps(self) returned no regions")
	}
	scannable := 0
	for _, r := range regions {
		if r.Scannable() {
			scannable++
		}
	}
	if scannable == 0 {
		t.Error("expected at least one scannable region in self")
	}
}
