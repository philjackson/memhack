package scan

import "testing"

func TestUndoMechanics(t *testing.T) {
	s := &Scanner{Type: I32}
	if s.CanUndo() {
		t.Fatal("new scanner should have nothing to undo")
	}
	if s.Undo() {
		t.Fatal("Undo on empty history should return false")
	}

	// Simulate a first scan: snapshot the pre-scan state, then install a
	// three-element match set.
	s.pushHistory()
	s.Matches = []Match{{Addr: 1}, {Addr: 2}, {Addr: 3}}
	s.scanned = true

	// Simulate a narrowing scan down to one match.
	s.pushHistory()
	s.Matches = []Match{{Addr: 2}}

	if !s.CanUndo() {
		t.Fatal("expected undo to be available")
	}

	// First undo: back to the three-element set.
	if !s.Undo() {
		t.Fatal("first undo returned false")
	}
	if len(s.Matches) != 3 || !s.scanned {
		t.Fatalf("after undo: %d matches, scanned=%v; want 3, true", len(s.Matches), s.scanned)
	}

	// Second undo: back to the pre-scan state.
	if !s.Undo() {
		t.Fatal("second undo returned false")
	}
	if len(s.Matches) != 0 || s.scanned {
		t.Fatalf("after second undo: %d matches, scanned=%v; want 0, false", len(s.Matches), s.scanned)
	}
	if s.CanUndo() {
		t.Fatal("history should be empty after undoing everything")
	}
}

func TestScannerAlignment(t *testing.T) {
	s := &Scanner{Type: I32}
	if got := s.Alignment(); got != 4 {
		t.Errorf("default i32 alignment = %d, want 4 (type width)", got)
	}
	s.Align = 1
	if got := s.Alignment(); got != 1 {
		t.Errorf("Align=1 alignment = %d, want 1", got)
	}
	s.Align = 8
	if got := s.Alignment(); got != 8 {
		t.Errorf("Align=8 alignment = %d, want 8", got)
	}
	// Variable-width types (Size 0) fall back to 1.
	b := &Scanner{Type: Bytes}
	if got := b.Alignment(); got != 1 {
		t.Errorf("bytes alignment = %d, want 1", got)
	}
}

func TestUndoHistoryCap(t *testing.T) {
	s := &Scanner{Type: I32}
	for i := 0; i < maxHistory+10; i++ {
		s.pushHistory()
	}
	if len(s.history) != maxHistory {
		t.Errorf("history length = %d, want capped at %d", len(s.history), maxHistory)
	}
}

func TestResetClearsHistory(t *testing.T) {
	s := &Scanner{Type: I32}
	s.pushHistory()
	s.pushHistory()
	if !s.CanUndo() {
		t.Fatal("expected undo available before reset")
	}
	s.Reset()
	if s.CanUndo() {
		t.Error("Reset should clear undo history")
	}
	if s.Matches != nil || s.scanned {
		t.Error("Reset should clear matches and scanned flag")
	}
}

func TestSnapshotsAreIndependent(t *testing.T) {
	// A snapshot must not be mutated by later changes to Scanner.Matches;
	// this guards the "narrow allocates a fresh slice" invariant.
	s := &Scanner{Type: I32}
	original := []Match{{Addr: 10}, {Addr: 20}}
	s.Matches = original
	s.scanned = true
	s.pushHistory()

	// Replace with a new slice (as narrow/first do).
	s.Matches = []Match{{Addr: 20}}

	if !s.Undo() {
		t.Fatal("undo failed")
	}
	if len(s.Matches) != 2 || s.Matches[0].Addr != 10 || s.Matches[1].Addr != 20 {
		t.Errorf("restored matches = %+v, want the original two", s.Matches)
	}
}
