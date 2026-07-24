package scan

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// DataType identifies how raw memory is interpreted for a scan. Most types are
// fixed-width numbers; Bytes and String are variable-width literal patterns.
type DataType int

const (
	I8 DataType = iota
	I16
	I32
	I64
	U8
	U16
	U32
	U64
	F32
	F64
	Bytes  // a literal byte pattern, entered as hex (e.g. "de ad be ef")
	String // a literal text pattern
)

// Size returns the width in bytes of the data type.
func (t DataType) Size() int {
	switch t {
	case I8, U8:
		return 1
	case I16, U16:
		return 2
	case I32, U32, F32:
		return 4
	case I64, U64, F64:
		return 8
	}
	return 0
}

func (t DataType) String() string {
	switch t {
	case I8:
		return "i8"
	case I16:
		return "i16"
	case I32:
		return "i32"
	case I64:
		return "i64"
	case U8:
		return "u8"
	case U16:
		return "u16"
	case U32:
		return "u32"
	case U64:
		return "u64"
	case F32:
		return "f32"
	case F64:
		return "f64"
	case Bytes:
		return "bytes"
	case String:
		return "string"
	}
	return "?"
}

// ParseDataType maps a type name to a DataType.
func ParseDataType(s string) (DataType, error) {
	switch s {
	case "i8", "int8":
		return I8, nil
	case "i16", "int16", "short":
		return I16, nil
	case "i32", "int32", "int":
		return I32, nil
	case "i64", "int64", "long":
		return I64, nil
	case "u8", "uint8", "byte":
		return U8, nil
	case "u16", "uint16":
		return U16, nil
	case "u32", "uint32":
		return U32, nil
	case "u64", "uint64":
		return U64, nil
	case "f32", "float", "float32":
		return F32, nil
	case "f64", "double", "float64":
		return F64, nil
	case "bytes", "byte-array", "aob":
		return Bytes, nil
	case "string", "str", "text":
		return String, nil
	}
	return 0, fmt.Errorf("unknown data type %q", s)
}

// IsFloat reports whether the type is a floating-point type.
func (t DataType) IsFloat() bool { return t == F32 || t == F64 }

// IsBytes reports whether the type is a variable-width literal pattern
// (Bytes or String) rather than a fixed-width number.
func (t DataType) IsBytes() bool { return t == Bytes || t == String }

// Decode interprets the leading Size() bytes of buf as a float64 for
// comparison purposes. Integers are widened losslessly enough for the
// value ranges scanmem-style tools care about.
func (t DataType) Decode(buf []byte) (float64, bool) {
	if len(buf) < t.Size() {
		return 0, false
	}
	switch t {
	case I8:
		return float64(int8(buf[0])), true
	case U8:
		return float64(buf[0]), true
	case I16:
		return float64(int16(binary.LittleEndian.Uint16(buf))), true
	case U16:
		return float64(binary.LittleEndian.Uint16(buf)), true
	case I32:
		return float64(int32(binary.LittleEndian.Uint32(buf))), true
	case U32:
		return float64(binary.LittleEndian.Uint32(buf)), true
	case I64:
		return float64(int64(binary.LittleEndian.Uint64(buf))), true
	case U64:
		return float64(binary.LittleEndian.Uint64(buf)), true
	case F32:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(buf))), true
	case F64:
		return math.Float64frombits(binary.LittleEndian.Uint64(buf)), true
	}
	return 0, false
}

// Encode converts a textual value into raw bytes of this type: little-endian
// for numbers, the literal pattern for Bytes/String.
func (t DataType) Encode(s string) ([]byte, error) {
	switch t {
	case String:
		return []byte(unquote(s)), nil
	case Bytes:
		return parseHexBytes(s)
	}

	buf := make([]byte, t.Size())
	if t.IsFloat() {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("parse float %q: %w", s, err)
		}
		switch t {
		case F32:
			binary.LittleEndian.PutUint32(buf, math.Float32bits(float32(v)))
		case F64:
			binary.LittleEndian.PutUint64(buf, math.Float64bits(v))
		}
		return buf, nil
	}

	// Integer types. Parse signed, then store the two's-complement bits.
	v, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		// Allow large unsigned literals too.
		u, uerr := strconv.ParseUint(s, 0, 64)
		if uerr != nil {
			return nil, fmt.Errorf("parse int %q: %w", s, err)
		}
		v = int64(u)
	}
	switch t {
	case I8, U8:
		buf[0] = byte(v)
	case I16, U16:
		binary.LittleEndian.PutUint16(buf, uint16(v))
	case I32, U32:
		binary.LittleEndian.PutUint32(buf, uint32(v))
	case I64, U64:
		binary.LittleEndian.PutUint64(buf, uint64(v))
	}
	return buf, nil
}

// Format renders the leading bytes of buf as a human-readable value.
func (t DataType) Format(buf []byte) string {
	if t.IsBytes() {
		return t.formatBytes(buf)
	}
	v, ok := t.Decode(buf)
	if !ok {
		return "?"
	}
	if t.IsFloat() {
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return strconv.FormatInt(int64(v), 10)
}

// formatDisplayCap bounds how many bytes Format renders for a byte/string
// value, so a long match doesn't blow out the display.
const formatDisplayCap = 32

func (t DataType) formatBytes(buf []byte) string {
	b, truncated := buf, false
	if len(b) > formatDisplayCap {
		b, truncated = b[:formatDisplayCap], true
	}
	var s string
	if t == String {
		r := make([]byte, len(b))
		for i, c := range b {
			if c >= 0x20 && c < 0x7f {
				r[i] = c
			} else {
				r[i] = '.' // render non-printable bytes as dots
			}
		}
		s = string(r)
	} else {
		parts := make([]string, len(b))
		for i, c := range b {
			parts[i] = fmt.Sprintf("%02x", c)
		}
		s = strings.Join(parts, " ")
	}
	if truncated {
		s += "…"
	}
	return s
}

// unquote strips a single pair of surrounding double quotes, so a literal
// string can preserve leading/trailing spaces or look like a keyword.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// parseHexBytes parses a hex byte pattern, ignoring spaces and ':'/'-'
// separators and an optional leading "0x".
func parseHexBytes(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', ':', '-':
			return -1
		}
		return r
	}, s)
	clean = strings.TrimPrefix(clean, "0x")
	clean = strings.TrimPrefix(clean, "0X")
	if clean == "" {
		return nil, fmt.Errorf("empty byte pattern")
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("invalid hex bytes %q: %w", s, err)
	}
	return b, nil
}
