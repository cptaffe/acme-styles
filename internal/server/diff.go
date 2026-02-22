package server

import "github.com/cptaffe/acme-styles/style"

// adjustRunsInsert shifts and extends layer runs for an insertion of n runes
// at q0.  Characters inserted strictly inside a run extend it; insertions at
// a boundary fall into the right neighbour.
func adjustRunsInsert(runs []style.StyleRun, q0, n int) {
	for i := range runs {
		r := &runs[i]
		switch {
		case q0 <= r.Start:
			r.Start += n
			r.End += n
		case q0 < r.End:
			r.End += n
		}
	}
}

// adjustRunsDelete applies deletion of runes [q0, q1) to a run slice,
// returning the updated slice.
func adjustRunsDelete(runs []style.StyleRun, q0, q1 int) []style.StyleRun {
	n := q1 - q0
	out := runs[:0]
	for _, r := range runs {
		switch {
		case r.End <= q0:
			out = append(out, r)
		case r.Start >= q1:
			out = append(out, style.StyleRun{Name: r.Name, Start: r.Start - n, End: r.End - n})
		case r.Start < q0 && r.End > q1:
			out = append(out, style.StyleRun{Name: r.Name, Start: r.Start, End: r.End - n})
		case r.Start < q0:
			if q0 > r.Start {
				out = append(out, style.StyleRun{Name: r.Name, Start: r.Start, End: q0})
			}
		case r.End > q1:
			out = append(out, style.StyleRun{Name: r.Name, Start: q0, End: r.End - n})
		// completely inside deletion: discard
		}
	}
	return out
}

// palettesEqual reports whether two palettes have the same named entries
// with identical visual definitions (order-insensitive).
func palettesEqual(a, b []style.PaletteEntry) bool {
	if len(a) != len(b) {
		return false
	}
	bm := make(map[string]style.PaletteEntry, len(b))
	for _, e := range b {
		bm[e.Name] = e
	}
	for _, e := range a {
		be, ok := bm[e.Name]
		if !ok || !e.Equal(be) {
			return false
		}
	}
	return true
}

// diffRuns finds the minimal dirty interval between two sorted,
// non-overlapping run slices.
func diffRuns(old, new []style.StyleRun) (q0, q1 int, changed bool) {
	i, j := 0, 0
	for i < len(old) && j < len(new) && old[i] == new[j] {
		i++
		j++
	}
	if i == len(old) && j == len(new) {
		return 0, 0, false
	}

	ei, ej := len(old)-1, len(new)-1
	for ei >= i && ej >= j && old[ei] == new[ej] {
		ei--
		ej--
	}

	const maxInt = int(^uint(0) >> 1)
	q0 = maxInt
	for _, r := range old[i : ei+1] {
		if r.Start < q0 {
			q0 = r.Start
		}
		if r.End > q1 {
			q1 = r.End
		}
	}
	for _, r := range new[j : ej+1] {
		if r.Start < q0 {
			q0 = r.Start
		}
		if r.End > q1 {
			q1 = r.End
		}
	}
	if q0 == maxInt || q0 >= q1 {
		return 0, 0, false
	}
	return q0, q1, true
}
