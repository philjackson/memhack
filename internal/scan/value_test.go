package scan

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// allTypes is every data type the scanner understands.
var allTypes = []DataType{I8, I16, I32, I64, U8, U16, U32, U64, F32, F64}

func TestDataTypeSize(t *testing.T) {
	want := map[DataType]int{
		I8: 1, U8: 1,
		I16: 2, U16: 2,
		I32: 4, U32: 4, F32: 4,
		I64: 8, U64: 8, F64: 8,
	}
	for _, typ := range allTypes {
		if got := typ.Size(); got != want[typ] {
			t.Errorf("%s.Size() = %d, want %d", typ, got, want[typ])
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	// At least one case per type. Values stay within float64's exact integer
	// range so the Encode->Decode round trip is lossless for every type.
	cases := []struct {
		typ DataType
		in  string
		out float64
	}{
		{I8, "-5", -5},
		{I8, "127", 127},
		{U8, "200", 200},
		{I16, "-1000", -1000},
		{I16, "32767", 32767},
		{U16, "60000", 60000},
		{I32, "1337", 1337},
		{I32, "-2147483648", -2147483648},
		{U32, "4000000000", 4000000000},
		{I64, "-9000000000", -9000000000},
		{U64, "9000000000", 9000000000},
		{F32, "3.5", 3.5},
		{F32, "-0.25", -0.25},
		{F64, "2.25", 2.25},
		{F64, "1e10", 1e10},
	}
	for _, c := range cases {
		raw, err := c.typ.Encode(c.in)
		if err != nil {
			t.Fatalf("%s.Encode(%q): %v", c.typ, c.in, err)
		}
		if len(raw) != c.typ.Size() {
			t.Errorf("%s.Encode(%q): got %d bytes, want %d", c.typ, c.in, len(raw), c.typ.Size())
		}
		got, ok := c.typ.Decode(raw)
		if !ok {
			t.Fatalf("%s.Decode failed", c.typ)
		}
		if got != c.out {
			t.Errorf("%s round trip %q: got %v, want %v", c.typ, c.in, got, c.out)
		}
	}
}

// TestEncodeBytes checks exact little-endian byte output for every type,
// including full-width extremes (e.g. max uint64) where a Decode-to-float64
// round trip would lose precision and hide an encoding bug.
func TestEncodeBytes(t *testing.T) {
	f32 := func(f float32) []byte {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, math.Float32bits(f))
		return b
	}
	f64 := func(f float64) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, math.Float64bits(f))
		return b
	}
	ff := func(n int) []byte { return bytes.Repeat([]byte{0xff}, n) }

	cases := []struct {
		typ  DataType
		in   string
		want []byte
	}{
		{I8, "-1", ff(1)},
		{U8, "255", ff(1)},
		{I16, "-1", ff(2)},
		{U16, "65535", ff(2)},
		{I32, "-1", ff(4)},
		{U32, "4294967295", ff(4)},
		{I64, "-1", ff(8)},
		{U64, "18446744073709551615", ff(8)},
		{U32, "1", []byte{0x01, 0x00, 0x00, 0x00}},
		{F32, "1.5", f32(1.5)},
		{F64, "1.5", f64(1.5)},
	}
	for _, c := range cases {
		raw, err := c.typ.Encode(c.in)
		if err != nil {
			t.Fatalf("%s.Encode(%q): %v", c.typ, c.in, err)
		}
		if !bytes.Equal(raw, c.want) {
			t.Errorf("%s.Encode(%q) = % x, want % x", c.typ, c.in, raw, c.want)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		typ      DataType
		in, want string
	}{
		{I8, "-5", "-5"},
		{U8, "200", "200"},
		{I16, "-1000", "-1000"},
		{U16, "60000", "60000"},
		{I32, "-1337", "-1337"},
		{U32, "4000000000", "4000000000"},
		{I64, "-9000000000", "-9000000000"},
		{U64, "9000000000", "9000000000"},
		{F32, "3.5", "3.5"},
		{F64, "2.25", "2.25"},
	}
	for _, c := range cases {
		raw, err := c.typ.Encode(c.in)
		if err != nil {
			t.Fatalf("%s.Encode(%q): %v", c.typ, c.in, err)
		}
		if got := c.typ.Format(raw); got != c.want {
			t.Errorf("%s.Format(Encode(%q)) = %q, want %q", c.typ, c.in, got, c.want)
		}
	}
}

func TestEncodeErrors(t *testing.T) {
	// Each type should reject clearly out-of-range or non-numeric input.
	cases := []struct {
		typ DataType
		in  string
	}{
		{I32, "notanumber"},
		{F64, "3.1.4"},
		{U8, ""},
	}
	for _, c := range cases {
		if _, err := c.typ.Encode(c.in); err == nil {
			t.Errorf("%s.Encode(%q): expected error", c.typ, c.in)
		}
	}
}

func TestDecodeShortBuffer(t *testing.T) {
	for _, typ := range allTypes {
		if typ.Size() <= 1 {
			continue
		}
		if _, ok := typ.Decode(make([]byte, typ.Size()-1)); ok {
			t.Errorf("%s.Decode(short buffer) = ok, want not ok", typ)
		}
	}
}

func TestParseDataType(t *testing.T) {
	// Every canonical name and alias must map to the expected type.
	want := map[string]DataType{
		"i8": I8, "int8": I8,
		"i16": I16, "int16": I16, "short": I16,
		"i32": I32, "int32": I32, "int": I32,
		"i64": I64, "int64": I64, "long": I64,
		"u8": U8, "uint8": U8, "byte": U8,
		"u16": U16, "uint16": U16,
		"u32": U32, "uint32": U32,
		"u64": U64, "uint64": U64,
		"f32": F32, "float": F32, "float32": F32,
		"f64": F64, "double": F64, "float64": F64,
	}
	for name, typ := range want {
		got, err := ParseDataType(name)
		if err != nil {
			t.Errorf("ParseDataType(%q): %v", name, err)
			continue
		}
		if got != typ {
			t.Errorf("ParseDataType(%q) = %s, want %s", name, got, typ)
		}
	}
	if _, err := ParseDataType("nope"); err == nil {
		t.Error("ParseDataType(nope): expected error")
	}
}

func TestDataTypeStringRoundTrip(t *testing.T) {
	// Each type's String() must parse back to the same type.
	for _, typ := range allTypes {
		got, err := ParseDataType(typ.String())
		if err != nil {
			t.Errorf("ParseDataType(%q): %v", typ.String(), err)
			continue
		}
		if got != typ {
			t.Errorf("%s.String() round trip = %s", typ, got)
		}
	}
}

func TestCondTest(t *testing.T) {
	cases := []struct {
		op    Op
		cur   float64
		prev  float64
		hp    bool
		val   float64
		val2  float64
		delta float64
		want  bool
	}{
		{op: Equal, cur: 10, val: 10, want: true},
		{op: Equal, cur: 10, val: 11, want: false},
		{op: Greater, cur: 10, val: 5, want: true},
		{op: Less, cur: 10, val: 5, want: false},
		{op: NotEqual, cur: 10, val: 5, want: true},
		{op: Increased, cur: 10, prev: 5, hp: true, want: true},
		{op: Increased, cur: 5, prev: 10, hp: true, want: false},
		{op: Decreased, cur: 5, prev: 10, hp: true, want: true},
		{op: Changed, cur: 5, prev: 5, hp: true, want: false},
		{op: Unchanged, cur: 5, prev: 5, hp: true, want: true},
		{op: Increased, cur: 10, prev: 5, hp: false, want: false}, // no prev -> false

		// Range: inclusive on both ends.
		{op: InRange, cur: 5, val: 1, val2: 10, want: true},
		{op: InRange, cur: 1, val: 1, val2: 10, want: true},
		{op: InRange, cur: 10, val: 1, val2: 10, want: true},
		{op: InRange, cur: 0, val: 1, val2: 10, want: false},
		{op: InRange, cur: 11, val: 1, val2: 10, want: false},

		// Increased/decreased by an exact amount.
		{op: IncreasedBy, cur: 15, prev: 10, hp: true, delta: 5, want: true},
		{op: IncreasedBy, cur: 16, prev: 10, hp: true, delta: 5, want: false},
		{op: IncreasedBy, cur: 15, prev: 10, hp: false, delta: 5, want: false}, // no prev
		{op: DecreasedBy, cur: 7, prev: 10, hp: true, delta: 3, want: true},
		{op: DecreasedBy, cur: 8, prev: 10, hp: true, delta: 3, want: false},
	}
	for i, c := range cases {
		cond := Cond{Op: c.op, Value: c.val, Value2: c.val2, Delta: c.delta}
		if got := cond.test(c.cur, c.prev, c.hp); got != c.want {
			t.Errorf("case %d op=%d: got %v want %v", i, c.op, got, c.want)
		}
	}
}
