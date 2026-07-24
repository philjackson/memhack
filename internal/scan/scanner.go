package scan

import (
	"fmt"

	"github.com/phil/memhack/internal/memory"
)

// Match is a single memory location that satisfied all scans so far, along
// with the value most recently observed there (used for relative scans).
type Match struct {
	Addr uint64
	Last []byte
}

// Op is a comparison operator for a scan.
type Op int

const (
	Equal Op = iota
	NotEqual
	Greater
	Less
	GreaterEqual
	LessEqual
	InRange // Value <= x <= Value2
	Changed
	Unchanged
	Increased
	Decreased
	IncreasedBy // x == prev + Delta
	DecreasedBy // x == prev - Delta
)

// Cond describes what a scan is looking for.
//
//   - Value-based ops (Equal, Greater, InRange, ...) compare against Value
//     (and Value2, the inclusive upper bound, for InRange).
//   - Relative ops (Changed, Increased, ...) compare against the value
//     previously stored at the same address. IncreasedBy/DecreasedBy also
//     use Delta as the exact amount of change.
type Cond struct {
	Op     Op
	Value  float64 // target, or lower bound for InRange
	Value2 float64 // inclusive upper bound for InRange
	Delta  float64 // exact change amount for IncreasedBy/DecreasedBy
}

// test evaluates the condition given the current and previous decoded values.
func (c Cond) test(cur, prev float64, havePrev bool) bool {
	switch c.Op {
	case Equal:
		return cur == c.Value
	case NotEqual:
		return cur != c.Value
	case Greater:
		return cur > c.Value
	case Less:
		return cur < c.Value
	case GreaterEqual:
		return cur >= c.Value
	case LessEqual:
		return cur <= c.Value
	case InRange:
		return cur >= c.Value && cur <= c.Value2
	case Changed:
		return havePrev && cur != prev
	case Unchanged:
		return havePrev && cur == prev
	case Increased:
		return havePrev && cur > prev
	case Decreased:
		return havePrev && cur < prev
	case IncreasedBy:
		return havePrev && cur == prev+c.Delta
	case DecreasedBy:
		return havePrev && cur == prev-c.Delta
	}
	return false
}

// needsPrev reports whether the op compares against a previous value.
func (o Op) needsPrev() bool {
	switch o {
	case Changed, Unchanged, Increased, Decreased, IncreasedBy, DecreasedBy:
		return true
	}
	return false
}

// maxHistory bounds how many prior match sets are retained for Undo. Each
// entry can be large (the first scan's result especially), so the depth is
// capped rather than unbounded.
const maxHistory = 16

// snapshot captures the scan state prior to a scan, for Undo.
type snapshot struct {
	matches []Match
	scanned bool
}

// Scanner holds scan state for one attached process and data type.
type Scanner struct {
	proc    *memory.Process
	Type    DataType
	Matches []Match
	scanned bool
	history []snapshot
}

// NewScanner creates a scanner for the given process and data type.
func NewScanner(proc *memory.Process, t DataType) *Scanner {
	return &Scanner{proc: proc, Type: t}
}

// SetType changes the data type and resets any accumulated matches. Because a
// new type reinterprets the same bytes, the undo history is dropped too.
func (s *Scanner) SetType(t DataType) {
	s.Type = t
	s.Reset()
}

// Reset discards all matches and undo history, returning to the pre-scan state.
func (s *Scanner) Reset() {
	s.Matches = nil
	s.scanned = false
	s.history = nil
}

// Scanned reports whether at least one scan has run since the last reset.
func (s *Scanner) Scanned() bool { return s.scanned }

// CanUndo reports whether there is a prior match set to return to.
func (s *Scanner) CanUndo() bool { return len(s.history) > 0 }

// Undo restores the match set to how it was before the most recent scan.
// It returns false if there is nothing to undo.
func (s *Scanner) Undo() bool {
	if len(s.history) == 0 {
		return false
	}
	last := s.history[len(s.history)-1]
	s.history = s.history[:len(s.history)-1]
	s.Matches = last.matches
	s.scanned = last.scanned
	return true
}

// pushHistory records the current match set so a later Undo can restore it.
func (s *Scanner) pushHistory() {
	s.history = append(s.history, snapshot{matches: s.Matches, scanned: s.scanned})
	if len(s.history) > maxHistory {
		// Drop the oldest entry.
		s.history = append(s.history[:0], s.history[1:]...)
	}
}

// chunkSize bounds how much we read from a region at once.
const chunkSize = 1 << 20 // 1 MiB

// Scan runs a scan with the given condition. The first scan sweeps all
// scannable regions; subsequent scans narrow the existing match set. The
// prior match set is saved first so the scan can be reverted with Undo.
func (s *Scanner) Scan(c Cond) error {
	s.pushHistory()
	var err error
	if s.scanned {
		err = s.narrow(c)
	} else {
		err = s.first(c)
	}
	if err != nil {
		// The scan didn't happen; drop the snapshot we just pushed so it
		// doesn't count as an undo step.
		s.history = s.history[:len(s.history)-1]
	}
	return err
}

// first performs the initial full-memory sweep.
func (s *Scanner) first(c Cond) error {
	if c.Op.needsPrev() {
		return fmt.Errorf("relative scan (changed/increased/...) needs a prior scan")
	}
	if err := s.proc.Freeze(); err != nil {
		return err
	}
	defer s.proc.Thaw()
	regions, err := memory.ReadMaps(s.proc.Pid)
	if err != nil {
		return err
	}
	size := s.Type.Size()
	var matches []Match
	buf := make([]byte, chunkSize)

	for _, r := range regions {
		if !r.Scannable() {
			continue
		}
		for addr := r.Start; addr < r.End; {
			n := chunkSize
			if remain := r.End - addr; uint64(n) > remain {
				n = int(remain)
			}
			got, err := s.proc.ReadAt(buf[:n], addr)
			if err != nil || got < size {
				// Unreadable slice (e.g. torn-down mapping); skip it.
				addr += uint64(n)
				continue
			}
			// Slide over the readable bytes with 1-byte granularity so we
			// find unaligned values, matching scanmem's default behaviour.
			for off := 0; off+size <= got; off++ {
				cur, ok := s.Type.Decode(buf[off : off+size])
				if !ok {
					continue
				}
				if c.test(cur, 0, false) {
					last := make([]byte, size)
					copy(last, buf[off:off+size])
					matches = append(matches, Match{Addr: addr + uint64(off), Last: last})
				}
			}
			addr += uint64(got)
		}
	}
	s.Matches = matches
	s.scanned = true
	return nil
}

// narrow re-reads each existing match and keeps those still satisfying c.
func (s *Scanner) narrow(c Cond) error {
	if err := s.proc.Freeze(); err != nil {
		return err
	}
	defer s.proc.Thaw()
	size := s.Type.Size()
	buf := make([]byte, size)
	// Build a fresh slice rather than filtering in place: the previous slice
	// is retained in the undo history and must not be overwritten.
	var kept []Match

	for _, m := range s.Matches {
		got, err := s.proc.ReadAt(buf, m.Addr)
		if err != nil || got < size {
			continue
		}
		cur, ok := s.Type.Decode(buf)
		if !ok {
			continue
		}
		prev, havePrev := s.Type.Decode(m.Last)
		if !c.test(cur, prev, havePrev) {
			continue
		}
		last := make([]byte, size)
		copy(last, buf)
		kept = append(kept, Match{Addr: m.Addr, Last: last})
	}
	s.Matches = kept
	return nil
}

// Refresh re-reads the current value at every match without filtering,
// updating the stored Last bytes.
func (s *Scanner) Refresh() { s.RefreshN(len(s.Matches)) }

// RefreshN re-reads the current value at up to the first n matches, updating
// their stored Last bytes. This lets a caller cheaply refresh only the subset
// it is displaying rather than every match.
func (s *Scanner) RefreshN(n int) {
	if n > len(s.Matches) {
		n = len(s.Matches)
	}
	if n <= 0 {
		return
	}
	if err := s.proc.Freeze(); err != nil {
		return
	}
	defer s.proc.Thaw()
	size := s.Type.Size()
	buf := make([]byte, size)
	for i := 0; i < n; i++ {
		if got, err := s.proc.ReadAt(buf, s.Matches[i].Addr); err == nil && got >= size {
			copy(s.Matches[i].Last, buf)
		}
	}
}

// Write stores raw bytes at the given match's address.
func (s *Scanner) Write(m Match, raw []byte) error {
	if err := s.proc.Freeze(); err != nil {
		return err
	}
	defer s.proc.Thaw()
	_, err := s.proc.WriteAt(raw, m.Addr)
	return err
}
