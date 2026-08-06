package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/phil/memhack/internal/scan"
)

func TestParseScanBytesAndString(t *testing.T) {
	// String mode: the whole line is a literal pattern.
	c, err := parseScan("PLAYER_ONE", scan.String)
	if err != nil {
		t.Fatalf("string parse: %v", err)
	}
	if c.Op != scan.Equal || string(c.Bytes) != "PLAYER_ONE" {
		t.Errorf("string parse = %+v", c)
	}

	// Quotes let a keyword be taken literally rather than as a relative op.
	c, _ = parseScan(`"changed"`, scan.String)
	if c.Op != scan.Equal || string(c.Bytes) != "changed" {
		t.Errorf("quoted string parse = %+v", c)
	}
	// Bare keyword is the relative op.
	if c, _ := parseScan("changed", scan.String); c.Op != scan.Changed {
		t.Errorf("bare 'changed' should be the Changed op, got %+v", c)
	}

	// Bytes mode: hex pattern.
	c, err = parseScan("de ad be ef", scan.Bytes)
	if err != nil {
		t.Fatalf("bytes parse: %v", err)
	}
	if c.Op != scan.Equal || !bytes.Equal(c.Bytes, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("bytes parse = %+v", c)
	}
	if _, err := parseScan("nothex", scan.Bytes); err == nil {
		t.Error("invalid hex should error in bytes mode")
	}
}

func TestParseAlign(t *testing.T) {
	cases := map[string]int{
		"type": 0, "auto": 0, "0": 0,
		"byte": 1, "none": 1, "1": 1,
		"4": 4, "8": 8,
	}
	for in, want := range cases {
		got, err := parseAlign(in)
		if err != nil {
			t.Errorf("parseAlign(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseAlign(%q) = %d, want %d", in, got, want)
		}
	}
	for _, in := range []string{"nope", "-2", "3.5"} {
		if _, err := parseAlign(in); err == nil {
			t.Errorf("parseAlign(%q): expected error", in)
		}
	}
}

func TestParseScan(t *testing.T) {
	tests := []struct {
		in        string
		wantOp    scan.Op
		wantVal   float64
		wantVal2  float64
		wantDelta float64
	}{
		{in: "1337", wantOp: scan.Equal, wantVal: 1337},
		{in: "=42", wantOp: scan.Equal, wantVal: 42},
		{in: "= 42", wantOp: scan.Equal, wantVal: 42},
		{in: "-5", wantOp: scan.Equal, wantVal: -5}, // glued minus = negative literal
		{in: "> 5", wantOp: scan.Greater, wantVal: 5},
		{in: ">10", wantOp: scan.Greater, wantVal: 10},
		{in: "< 2", wantOp: scan.Less, wantVal: 2},
		{in: ">= 100", wantOp: scan.GreaterEqual, wantVal: 100},
		{in: "<=7", wantOp: scan.LessEqual, wantVal: 7},
		{in: "!= 9", wantOp: scan.NotEqual, wantVal: 9},
		{in: "changed", wantOp: scan.Changed},
		{in: "unchanged", wantOp: scan.Unchanged},
		{in: "inc", wantOp: scan.Increased},
		{in: "increased", wantOp: scan.Increased},
		{in: "+", wantOp: scan.Increased},
		{in: "dec", wantOp: scan.Decreased},
		{in: "-", wantOp: scan.Decreased},

		// Range.
		{in: "42..99", wantOp: scan.InRange, wantVal: 42, wantVal2: 99},
		{in: "10 .. 20", wantOp: scan.InRange, wantVal: 10, wantVal2: 20},
		{in: "-10..10", wantOp: scan.InRange, wantVal: -10, wantVal2: 10},
		{in: "20..10", wantOp: scan.InRange, wantVal: 10, wantVal2: 20}, // swapped

		// Change by an exact amount (separated operator / keyword).
		{in: "+ 5", wantOp: scan.IncreasedBy, wantDelta: 5},
		{in: "inc 5", wantOp: scan.IncreasedBy, wantDelta: 5},
		{in: "- 3", wantOp: scan.DecreasedBy, wantDelta: 3},
		{in: "dec 3", wantOp: scan.DecreasedBy, wantDelta: 3},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			cond, err := parseScan(tt.in, scan.I32)
			if err != nil {
				t.Fatalf("parseScan(%q): %v", tt.in, err)
			}
			if cond.Op != tt.wantOp {
				t.Errorf("op = %d, want %d", cond.Op, tt.wantOp)
			}
			if cond.Value != tt.wantVal {
				t.Errorf("value = %v, want %v", cond.Value, tt.wantVal)
			}
			if cond.Value2 != tt.wantVal2 {
				t.Errorf("value2 = %v, want %v", cond.Value2, tt.wantVal2)
			}
			if cond.Delta != tt.wantDelta {
				t.Errorf("delta = %v, want %v", cond.Delta, tt.wantDelta)
			}
		})
	}
}

func TestParseScanFloat(t *testing.T) {
	cond, err := parseScan("> 3.5", scan.F32)
	if err != nil {
		t.Fatalf("parseScan float: %v", err)
	}
	if cond.Op != scan.Greater || cond.Value != 3.5 {
		t.Errorf("got op=%d val=%v, want Greater 3.5", cond.Op, cond.Value)
	}
}

func TestParseScanErrors(t *testing.T) {
	for _, in := range []string{"abc", "> notanumber", "=", ">="} {
		if _, err := parseScan(in, scan.I32); err == nil {
			t.Errorf("parseScan(%q): expected error", in)
		}
	}
}

func TestFormatMatch(t *testing.T) {
	a := &app{tabSet: newTabSet(scan.I32, 0)}
	raw, _ := scan.I32.Encode("1337")
	got := a.formatMatch(2, scan.Match{Addr: 0x55c8e8e22030, Last: raw})
	want := "[2] 0x55c8e8e22030 = 1337"
	if got != want {
		t.Errorf("formatMatch = %q, want %q", got, want)
	}

	// A different type must reinterpret the same bytes.
	af := &app{tabSet: newTabSet(scan.F32, 0)}
	fraw, _ := scan.F32.Encode("1.5")
	if got := af.formatMatch(0, scan.Match{Addr: 0x1000, Last: fraw}); got != "[0] 0x000000001000 = 1.5" {
		t.Errorf("float formatMatch = %q", got)
	}
}

func TestParseInterval(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"250ms", 250 * time.Millisecond},
		{"1s", time.Second},
		{"2", 2 * time.Millisecond},
		{"500", 500 * time.Millisecond},
	}
	for _, tt := range tests {
		got, err := parseInterval(tt.in)
		if err != nil {
			t.Errorf("parseInterval(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseInterval(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	if _, err := parseInterval("nonsense"); err == nil {
		t.Error("parseInterval(nonsense): expected error")
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.n); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
