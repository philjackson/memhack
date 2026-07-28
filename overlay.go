package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// padBetween lays left and right out on one line w cells wide, with the gap
// between them, keeping at least one space so they never run together.
func padBetween(left, right string, w int) string {
	gap := w - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// placeOverlayCenter composites box over the centre of base and returns the
// result with base's dimensions unchanged: each covered line is cut around the
// box so it appears to float above the content underneath rather than pushing
// it around. width is the screen width to centre within; <= 0 centres within
// base's own widest line instead. If box does not fit, base is returned
// untouched.
func placeOverlayCenter(base, box string, width int) string {
	baseLines := strings.Split(base, "\n")
	boxLines := strings.Split(box, "\n")
	if len(boxLines) > len(baseLines) {
		return base
	}

	baseW := width
	if baseW <= 0 {
		for _, l := range baseLines {
			if w := ansi.StringWidth(l); w > baseW {
				baseW = w
			}
		}
	}
	boxW := 0
	for _, l := range boxLines {
		if w := ansi.StringWidth(l); w > boxW {
			boxW = w
		}
	}
	if boxW > baseW {
		return base
	}

	x := (baseW - boxW) / 2
	y := (len(baseLines) - len(boxLines)) / 2
	for i, boxLine := range boxLines {
		line := baseLines[y+i]

		// Left of the box: pad short lines out to the box's column.
		left := ansi.Truncate(line, x, "")
		if pad := x - ansi.StringWidth(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		// The box itself, padded to a uniform width so the right-hand remnant
		// of every covered line resumes at the same column.
		if pad := boxW - ansi.StringWidth(boxLine); pad > 0 {
			boxLine += strings.Repeat(" ", pad)
		}
		// Right of the box: whatever of the line extends past it.
		right := ansi.TruncateLeft(line, x+boxW, "")

		baseLines[y+i] = left + boxLine + right
	}
	return strings.Join(baseLines, "\n")
}
