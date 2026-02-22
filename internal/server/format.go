package server

import "github.com/cptaffe/acme-styles/style"

// formatAt returns the wire-format representation of palette and the subset
// of runs that overlap [q0, q1), with run offsets expressed relative to q0.
// It is used for partial addr-scoped writes to acme when only the palette
// is unchanged and the dirty interval is known.
func formatAt(palette []style.PaletteEntry, runs []style.StyleRun, q0, q1 int) string {
	out := make([]style.StyleRun, 0, len(runs))
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
		out = append(out, style.StyleRun{Name: r.Name, Start: start - q0, End: end - q0})
	}
	return style.Format(palette, out)
}
