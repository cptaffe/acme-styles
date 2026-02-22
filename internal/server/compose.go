package server

import (
	"sort"
	"strconv"

	"github.com/cptaffe/acme-styles/style"
)

// layerSortKey returns the sort priority for a layer name given the order
// list.  Lower index = higher priority in the order slice, but compose()
// wants higher-priority layers to win in the sweep, so callers sort
// descending by key.
func layerSortKey(order []string, name string) int {
	wildcard := -1
	for i, n := range order {
		if n == "*" {
			wildcard = i
		} else if n == name {
			return i
		}
	}
	if wildcard >= 0 {
		return wildcard
	}
	return len(order)
}

// mergePalettes merges the master palette with each layer's palette,
// returning the merged palette and per-layer rename maps.  If a layer
// defines an entry whose name already exists with a different visual
// definition, the entry is suffixed with "_<layerID>" to avoid collision.
func mergePalettes(master []style.PaletteEntry, layers []*Layer) ([]style.PaletteEntry, map[int]map[string]string) {
	out := append([]style.PaletteEntry{}, master...)
	renameMaps := make(map[int]map[string]string)

	for _, l := range layers {
		rm := make(map[string]string)
		renameMaps[l.ID] = rm

		for _, e := range l.Palette {
			origName := e.Name
			existingIdx := -1
			for i, oe := range out {
				if oe.Name == origName {
					existingIdx = i
					break
				}
			}
			if existingIdx < 0 {
				out = append(out, e)
				rm[origName] = origName
			} else if out[existingIdx].Equal(e) {
				rm[origName] = origName
			} else {
				newName := origName + "_" + strconv.Itoa(l.ID)
				e.Name = newName
				out = append(out, e)
				rm[origName] = newName
			}
		}
	}
	return out, renameMaps
}

// compose merges the master palette with all layers and produces a sorted,
// non-overlapping list of style runs.  Layers are sorted by their priority
// in order (lower index = higher priority); ties are broken by layer ID
// (lower ID = higher priority).  Runs from higher-priority layers occlude
// lower-priority ones at overlapping positions.
func compose(master []style.PaletteEntry, order []string, layers []*Layer) ([]style.PaletteEntry, []style.StyleRun) {
	if len(layers) == 0 {
		return master, nil
	}

	sorted := make([]*Layer, len(layers))
	copy(sorted, layers)
	sort.SliceStable(sorted, func(i, j int) bool {
		ki := layerSortKey(order, sorted[i].Name)
		kj := layerSortKey(order, sorted[j].Name)
		if ki != kj {
			return ki > kj
		}
		return sorted[i].ID < sorted[j].ID
	})
	layers = sorted

	palette, renameMaps := mergePalettes(master, layers)

	// Build a set of known palette names so that runs referencing undefined
	// entries can be skipped.  An undefined run would otherwise override a
	// lower-priority layer's run with "base" (the fall-through in winparsestyle)
	// rather than being transparent.  For example, an LSP "namespace" token
	// that has no palette entry should not occlude treesitter's "string" span.
	paletteNames := make(map[string]bool, len(palette))
	for _, e := range palette {
		paletteNames[e.Name] = true
	}

	type event struct {
		pos      int
		layerIdx int
		name     string
		isEnd    bool
	}
	var events []event

	for li, l := range layers {
		rm := renameMaps[l.ID]
		for _, r := range l.Runs {
			if r.End <= r.Start || r.Name == "" {
				continue
			}
			name := r.Name
			if rm != nil {
				if renamed, ok := rm[r.Name]; ok {
					name = renamed
				}
			}
			// Skip runs whose style name is not in the merged palette —
			// they have no visual definition and should be transparent.
			if !paletteNames[name] {
				continue
			}
			events = append(events, event{r.Start, li, name, false})
			events = append(events, event{r.End, li, name, true})
		}
	}

	if len(events) == 0 {
		return palette, nil
	}

	sort.Slice(events, func(i, j int) bool {
		if events[i].pos != events[j].pos {
			return events[i].pos < events[j].pos
		}
		if events[i].isEnd != events[j].isEnd {
			return events[i].isEnd
		}
		return events[i].layerIdx < events[j].layerIdx
	})

	active := make(map[int]string)
	bestName := func() string {
		best, bestLayer := "", -1
		for lay, name := range active {
			if name != "" && lay > bestLayer {
				best, bestLayer = name, lay
			}
		}
		return best
	}

	var result []style.StyleRun
	curName, curPos := "", 0

	for i := 0; i < len(events); {
		pos := events[i].pos
		if pos > curPos && curName != "" {
			result = append(result, style.StyleRun{Name: curName, Start: curPos, End: pos})
		}
		for i < len(events) && events[i].pos == pos {
			ev := events[i]
			if ev.isEnd {
				delete(active, ev.layerIdx)
			} else {
				active[ev.layerIdx] = ev.name
			}
			i++
		}
		curPos = pos
		curName = bestName()
	}
	return palette, result
}
