package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestPlaceOverlayCenterKeepsDimensions(t *testing.T) {
	base := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", 40)+"\n", 10), "\n")
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Render("hi")

	out := placeOverlayCenter(base, box, 40)
	lines := strings.Split(out, "\n")
	if len(lines) != 10 {
		t.Fatalf("overlay changed the line count: got %d, want 10", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != 40 {
			t.Errorf("line %d width = %d, want 40", i, w)
		}
	}
	if !strings.Contains(out, "hi") {
		t.Error("overlay content missing from the result")
	}
	// The box sits in the middle: the first and last lines are untouched.
	if lines[0] != strings.Repeat("x", 40) || lines[9] != strings.Repeat("x", 40) {
		t.Error("overlay should not disturb the top and bottom lines")
	}
	// Content underneath survives to the left and right of the box.
	mid := lines[5]
	if !strings.HasPrefix(mid, "xx") || !strings.HasSuffix(mid, "xx") {
		t.Errorf("covered line should keep its edges: %q", mid)
	}
}

func TestPlaceOverlayCenterIgnoresBoxThatDoesNotFit(t *testing.T) {
	base := "aaaa\nbbbb\ncccc"
	if got := placeOverlayCenter(base, "wide box that overflows", 4); got != base {
		t.Error("a box wider than the screen should be dropped, not clipped")
	}
	tall := "1\n2\n3\n4\n5\n6"
	if got := placeOverlayCenter(base, tall, 4); got != base {
		t.Error("a box taller than the base should be dropped")
	}
}

// Styled base content must not lose its cells around the box.
func TestPlaceOverlayCenterWithStyledBase(t *testing.T) {
	style := lipgloss.NewStyle().Bold(true)
	row := style.Render(strings.Repeat("y", 30))
	base := strings.TrimSuffix(strings.Repeat(row+"\n", 8), "\n")

	out := placeOverlayCenter(base, "[busy]", 30)
	for i, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w != 30 {
			t.Errorf("styled line %d width = %d, want 30", i, w)
		}
	}
	if !strings.Contains(out, "[busy]") {
		t.Error("overlay content missing over styled base")
	}
}
