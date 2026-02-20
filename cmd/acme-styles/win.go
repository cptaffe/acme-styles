package main

import (
	"fmt"
	"strings"
)

// formatStyleFull formats a full palette + run table for writing to N/style.
func formatStyleFull(palette []PaletteEntry, runs []StyleRun) string {
	var sb strings.Builder
	for _, e := range palette {
		writePaletteLine(&sb, e)
	}
	for _, r := range runs {
		fmt.Fprintf(&sb, "%d %d %s\n", r.Start, r.End-r.Start, r.Name)
	}
	return sb.String()
}

// formatStyleAt formats a palette + the subset of runs that fall within
// [q0, q1), with rune offsets relative to q0, for a partial addr-scoped write.
func formatStyleAt(palette []PaletteEntry, runs []StyleRun, q0, q1 int) string {
	var sb strings.Builder
	for _, e := range palette {
		writePaletteLine(&sb, e)
	}
	for _, r := range runs {
		if r.End <= q0 || r.Start >= q1 {
			continue
		}
		start := r.Start
		if start < q0 {
			start = q0
		}
		end := r.End
		if end > q1 {
			end = q1
		}
		fmt.Fprintf(&sb, "%d %d %s\n", start-q0, end-start, r.Name)
	}
	return sb.String()
}

func writePaletteLine(sb *strings.Builder, e PaletteEntry) {
	fmt.Fprintf(sb, ":%s", e.Name)
	if e.FontName != "" {
		fmt.Fprintf(sb, " font=%s", e.FontName)
	}
	if e.FG != "" {
		fmt.Fprintf(sb, " fg=%s", e.FG)
	}
	if e.BG != "" {
		fmt.Fprintf(sb, " bg=%s", e.BG)
	}
	if e.Bold {
		sb.WriteString(" bold")
	}
	if e.Italic {
		sb.WriteString(" italic")
	}
	if e.Underline {
		sb.WriteString(" underline")
	}
	sb.WriteByte('\n')
}
