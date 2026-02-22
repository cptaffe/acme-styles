// Package style defines the shared wire-format types for the acme-styles
// compositor protocol.
//
// PaletteEntry and StyleRun are used by both the compositor daemon and client
// tools that submit highlight spans through the compositor.  Tools that supply
// their own per-layer palette definitions (rather than relying on the master
// palette) can construct PaletteEntry values and serialise them with Format.
package style

import (
	"fmt"
	"strings"
)

// PaletteEntry is a named visual style definition.
type PaletteEntry struct {
	Name      string // e.g. "keyword"
	FontName  string // absolute font path, or ""
	FG        string // "#rrggbb", or ""
	BG        string // "#rrggbb", or ""
	Bold      bool
	Italic    bool
	Underline bool
}

// Equal reports whether e and b have identical visual properties (all fields
// except Name).
func (e PaletteEntry) Equal(b PaletteEntry) bool {
	return e.FontName == b.FontName &&
		e.FG == b.FG &&
		e.BG == b.BG &&
		e.Bold == b.Bold &&
		e.Italic == b.Italic &&
		e.Underline == b.Underline
}

// StyleRun is a named style span.  Start and End are file-absolute rune
// offsets; End is exclusive.
type StyleRun struct {
	Name  string
	Start int
	End   int // exclusive
}

// Format serialises palette entries and style runs into the acme-styles wire
// format, suitable for passing to StyleLayer.Write or StyleLayer.WriteAt.
func Format(palette []PaletteEntry, runs []StyleRun) string {
	var sb strings.Builder
	for _, e := range palette {
		writePaletteLine(&sb, e)
	}
	for _, r := range runs {
		fmt.Fprintf(&sb, "%d %d %s\n", r.Start, r.End-r.Start, r.Name)
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
