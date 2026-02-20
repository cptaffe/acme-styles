package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"9fans.net/go/acme"
)

// PaletteEntry is one named style definition within a layer's palette.
type PaletteEntry struct {
	Name      string // e.g. "keyword"
	FontName  string // absolute font path, or ""
	FG        string // "#rrggbb", or ""
	BG        string // "#rrggbb", or ""
	Bold      bool
	Italic    bool
	Underline bool
}

// StyleRun is a named style span.  Start and End are file-absolute rune
// offsets; End is exclusive.
type StyleRun struct {
	Name  string
	Start int
	End   int // exclusive
}

// Layer is a named collection of palette entries and style runs.
type Layer struct {
	ID       int
	Name     string
	Priority int // higher value wins in composition; default 0
	Palette  []PaletteEntry
	Runs     []StyleRun
}

// coalesceDelay is the window during which multiple flushLayer calls are
// batched into a single write to acme.  Multiple tools (lsp, treesitter)
// often flush within milliseconds of each other; coalescing prevents
// redundant winframesync calls in acme.
const coalesceDelay = 20 * time.Millisecond

// flushMsg carries a composed palette+run result from flushLayer to the
// per-window flusher goroutine.
type flushMsg struct {
	pal  []PaletteEntry
	runs []StyleRun
}

// WinState holds all layers for one acme window.
type WinState struct {
	ID     int
	mu     sync.Mutex
	layers []*Layer
	nextID int
	// win is the acme window handle used for Addr and Style writes.
	win *acme.Win
	// prevPalette and prevRuns are the last composition result written to acme.
	// They are kept in sync with body edits via insertAt/deleteRange so that
	// diffRuns can produce tight dirty intervals.  Protected by mu.
	prevPalette []PaletteEntry
	prevRuns    []StyleRun
	// flushCh delivers new compositions to the flusher goroutine.
	// Buffered so flushLayer never blocks in the common case.
	flushCh chan flushMsg
	// ctx/cancel are the per-window context; cancel stops the flusher when
	// the window closes or the server shuts down.
	ctx    context.Context
	cancel context.CancelFunc
}

// Config holds all values parsed from the styles file.
type Config struct {
	// Palette is the master set of named style definitions prepended to
	// every composite write sent to acme.
	Palette []PaletteEntry

	// LayerOrder lists layer names from highest priority (index 0) to
	// lowest.  A "*" entry is the wildcard slot for layers not explicitly
	// named; omitting "*" places unnamed layers at highest priority.
	LayerOrder []string
}

// Server is the global service state.
type Server struct {
	mu            sync.Mutex
	masterPalette []PaletteEntry
	layerOrder    []string
	wins          map[int]*WinState
	ctx           context.Context // root context; cancelled on shutdown
}

func newServer(cfg Config, ctx context.Context) *Server {
	return &Server{
		masterPalette: cfg.Palette,
		layerOrder:    cfg.LayerOrder,
		wins:          make(map[int]*WinState),
		ctx:           ctx,
	}
}

func (s *Server) addWin(id int) *WinState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.wins[id]; !ok {
		ctx, cancel := context.WithCancel(s.ctx)
		awin, err := acme.Open(id, nil)
		if err != nil {
			log.Printf("addWin %d: %v", id, err)
		}
		ws := &WinState{
			ID:      id,
			nextID:  1,
			win:     awin,
			flushCh: make(chan flushMsg, 8),
			ctx:     ctx,
			cancel:  cancel,
		}
		go ws.flusher(ctx)
		s.wins[id] = ws
		return ws
	}
	return nil
}

// clearStyle sends a zero-byte style write to the window, triggering
// winclearstyle + winframesync in acme and removing any stale styles from a
// previous acme-styles run.
func (ws *WinState) clearStyle() {
	if ws.win == nil {
		return
	}
	if err := ws.win.Style(nil); err != nil {
		log.Printf("win %d: clear style: %v", ws.ID, err)
	}
}

func (s *Server) delWin(id int) {
	s.mu.Lock()
	ws := s.wins[id]
	delete(s.wins, id)
	s.mu.Unlock()
	if ws != nil {
		if ws.win != nil {
			ws.win.CloseFiles()
		}
		ws.cancel()
	}
}

func (s *Server) getWin(id int) *WinState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wins[id]
}

func (s *Server) winIDs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int, 0, len(s.wins))
	for id := range s.wins {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// masterPaletteCopy returns a safe snapshot of the master palette.
func (s *Server) masterPaletteCopy() []PaletteEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PaletteEntry, len(s.masterPalette))
	copy(out, s.masterPalette)
	return out
}

// layerOrderCopy returns a safe snapshot of the layer order list.
func (s *Server) layerOrderCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.layerOrder))
	copy(out, s.layerOrder)
	return out
}

// ---- layer management ----

func (w *WinState) newLayer() *Layer {
	w.mu.Lock()
	defer w.mu.Unlock()
	l := &Layer{ID: w.nextID, Name: strconv.Itoa(w.nextID)}
	w.layers = append(w.layers, l)
	w.nextID++
	return l
}

func (w *WinState) getLayer(id int) *Layer {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range w.layers {
		if l.ID == id {
			return l
		}
	}
	return nil
}

func (w *WinState) layerIDs() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]int, 0, len(w.layers))
	for _, l := range w.layers {
		ids = append(ids, l.ID)
	}
	sort.Ints(ids)
	return ids
}

func (w *WinState) indexText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var sb strings.Builder
	for _, l := range w.layers {
		fmt.Fprintf(&sb, "%d %s\n", l.ID, l.Name)
	}
	return sb.String()
}

func (w *WinState) styleText(layID int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range w.layers {
		if l.ID == layID {
			return serializeLayer(l)
		}
	}
	return ""
}

func (w *WinState) setLayerName(layID int, name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range w.layers {
		if l.ID == layID {
			l.Name = name
			return
		}
	}
}

// clearLayer removes all palette entries and runs from the given layer.
// Compose / writeStyle are deferred to flushLayer at fid clunk.
func (w *WinState) clearLayer(layID int) {
	w.mu.Lock()
	for _, l := range w.layers {
		if l.ID == layID {
			l.Palette = nil
			l.Runs = nil
			break
		}
	}
	w.mu.Unlock()
}

// delLayer removes the layer entirely from the window.
// Compose / writeStyle are deferred to flushLayer at fid clunk.
func (w *WinState) delLayer(layID int) {
	w.mu.Lock()
	for i, l := range w.layers {
		if l.ID == layID {
			w.layers = append(w.layers[:i], w.layers[i+1:]...)
			break
		}
	}
	w.mu.Unlock()
}

// ---- edit tracking ----

// adjustRuns applies an insertion of n runes at q0 to a run slice in place.
// adjustRunsInsert shifts and extends layer runs for an insertion of n runes
// at q0.  Characters inserted strictly inside a run extend it; insertions at
// a run boundary or in a gap leave existing runs untouched — the compositor
// will resync on the next flush from acme-lsp or acme-treesitter.
func adjustRunsInsert(runs []StyleRun, q0, n int) {
	for i := range runs {
		r := &runs[i]
		switch {
		case q0 <= r.Start:
			// At or before the run: shift it right.
			r.Start += n
			r.End += n
		case q0 < r.End:
			// Interior: extend the run.
			r.End += n
		// q0 >= r.End: run is entirely to the left; leave it.
		}
	}
}

// insertAt adjusts all layer run positions for n runes inserted at q0.
// prevRuns is kept in sync so diffRuns sees accurate positions next flush.
func (ws *WinState) insertAt(q0, n int) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, l := range ws.layers {
		adjustRunsInsert(l.Runs, q0, n)
	}
	adjustRunsInsert(ws.prevRuns, q0, n)
}

// adjustRunsDelete applies deletion of runes [q0, q1) to a run slice,
// returning the updated slice.
func adjustRunsDelete(runs []StyleRun, q0, q1 int) []StyleRun {
	n := q1 - q0
	out := runs[:0]
	for _, r := range runs {
		switch {
		case r.End <= q0:
			out = append(out, r)
		case r.Start >= q1:
			out = append(out, StyleRun{r.Name, r.Start - n, r.End - n})
		case r.Start < q0 && r.End > q1:
			out = append(out, StyleRun{r.Name, r.Start, r.End - n})
		case r.Start < q0:
			if q0 > r.Start {
				out = append(out, StyleRun{r.Name, r.Start, q0})
			}
		case r.End > q1:
			out = append(out, StyleRun{r.Name, q0, r.End - n})
		// completely inside deletion: discard
		}
	}
	return out
}

// deleteRange adjusts all layer run positions for runes [q0, q1) removed.
// prevRuns is kept in sync so diffRuns sees accurate positions next flush.
func (ws *WinState) deleteRange(q0, q1 int) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, l := range ws.layers {
		l.Runs = adjustRunsDelete(l.Runs, q0, q1)
	}
	ws.prevRuns = adjustRunsDelete(ws.prevRuns, q0, q1)
}

// ---- parsing ----

// parsePaletteLine parses "name [prop ...]" (after the leading ':' is stripped).
func parsePaletteLine(line string) (PaletteEntry, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return PaletteEntry{}, false
	}
	e := PaletteEntry{Name: fields[0]}
	for _, tok := range fields[1:] {
		switch {
		case tok == "bold":
			e.Bold = true
		case tok == "italic":
			e.Italic = true
		case tok == "underline":
			e.Underline = true
		case strings.HasPrefix(tok, "font="):
			e.FontName = tok[5:]
		case strings.HasPrefix(tok, "fg="):
			e.FG = tok[3:]
		case strings.HasPrefix(tok, "bg="):
			e.BG = tok[3:]
		}
	}
	return e, true
}

// parseRunLine parses "start length name".
func parseRunLine(line string) (StyleRun, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return StyleRun{}, false
	}
	start, err := strconv.Atoi(fields[0])
	if err != nil {
		return StyleRun{}, false
	}
	length, err := strconv.Atoi(fields[1])
	if err != nil || length <= 0 {
		return StyleRun{}, false
	}
	return StyleRun{Name: fields[2], Start: start, End: start + length}, true
}

// ParseConfig parses the master styles file into a Config.
//
// Lines starting with ':' are palette entries.
// Lines starting with '@' define the layer stacking order, one name per
// line, highest priority first.  '*' is the wildcard slot for layers not
// explicitly listed.  Lines starting with '#' and blank lines are ignored.
//
// Example:
//
//	@ highlight
//	@ *
//	@ lsp
//	@ treesitter
//	:base fg=#000000 bg=#ffffff
//	:keyword fg=#569cd6 bold
func ParseConfig(content string) Config {
	var cfg Config
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
			if e, ok := parsePaletteLine(line[1:]); ok {
				cfg.Palette = append(cfg.Palette, e)
			}
		case strings.HasPrefix(line, "@"):
			if name := strings.TrimSpace(line[1:]); name != "" {
				cfg.LayerOrder = append(cfg.LayerOrder, name)
			}
		}
	}
	return cfg
}

// parseStyleWrite processes accumulated write bytes for a layer style file.
// Complete lines are parsed and added to the layer; the incomplete trailing
// line stays in *wbuf for the next write call.
func (w *WinState) parseStyleWrite(layID int, wbuf *[]byte) error {
	buf := *wbuf
	for {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(string(buf[:nl]))
		buf = buf[nl+1:]
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, ":") {
			e, ok := parsePaletteLine(line[1:])
			if !ok {
				continue
			}
			w.mu.Lock()
			for _, l := range w.layers {
				if l.ID == layID {
					l.Palette = append(l.Palette, e)
					break
				}
			}
			w.mu.Unlock()
		} else {
			r, ok := parseRunLine(line)
			if !ok {
				continue
			}
			w.mu.Lock()
			for _, l := range w.layers {
				if l.ID == layID {
					l.Runs = append(l.Runs, r)
					break
				}
			}
			w.mu.Unlock()
		}
	}
	*wbuf = buf
	return nil
}

// ---- serialization ----


func serializeLayer(l *Layer) string {
	var sb strings.Builder
	for _, e := range l.Palette {
		writePaletteLine(&sb, e)
	}
	for _, r := range l.Runs {
		fmt.Fprintf(&sb, "%d %d %s\n", r.Start, r.End-r.Start, r.Name)
	}
	return sb.String()
}

// ---- composition ----

// paletteIdentical reports whether two entries have the same visual definition.
func paletteIdentical(a, b PaletteEntry) bool {
	return a.FontName == b.FontName &&
		a.FG == b.FG &&
		a.BG == b.BG &&
		a.Bold == b.Bold &&
		a.Italic == b.Italic &&
		a.Underline == b.Underline
}

// mergePalettes merges the master palette with each layer's palette.
// Returns the combined palette and, for each layer ID, a map translating
// original palette-entry names to their (possibly renamed) output names.
//
// Conflict rule: if a layer's entry shares a name with an existing entry in
// the merged set but has different visual properties, it is renamed to
// name_<layerID>.  Identical definitions are de-duplicated.
func mergePalettes(master []PaletteEntry, layers []*Layer) ([]PaletteEntry, map[int]map[string]string) {
	out := append([]PaletteEntry{}, master...)
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
				// New name: add as-is.
				out = append(out, e)
				rm[origName] = origName
			} else if paletteIdentical(out[existingIdx], e) {
				// Same definition: no conflict.
				rm[origName] = origName
			} else {
				// Conflict: rename to name_layerID.
				newName := origName + "_" + strconv.Itoa(l.ID)
				e.Name = newName
				out = append(out, e)
				rm[origName] = newName
			}
		}
	}
	return out, renameMaps
}

// layerSortKey returns the sort key for a layer given the order list.
// order[0] is highest priority; layers should end up at the highest slice
// index in compose so the sweep-line picks them.  We therefore sort
// descending by order position (larger position = lower priority = earlier
// in slice = lower layerIdx = loses).  Unknown layers land at the '*'
// wildcard position; if there is no '*' they go last (highest priority).
func layerSortKey(order []string, name string) int {
	wildcard := -1 // no '*' found yet → unknown layers go last
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
	return len(order) // after all listed entries → highest priority
}

// compose merges the master palette with all layers and produces a sorted,
// non-overlapping list of style runs.  Layers are ordered according to the
// order slice (index 0 = highest priority; wins in the sweep-line).
func compose(master []PaletteEntry, order []string, layers []*Layer) ([]PaletteEntry, []StyleRun) {
	if len(layers) == 0 {
		return master, nil
	}

	// Sort layers so that the highest-priority layer ends up at the highest
	// slice index (sweep-line picks highest layerIdx).
	// Descending sort key means: larger key → appears earlier in sorted
	// slice → lower layerIdx → loses.  Lowest key → last → highest layerIdx → wins.
	sorted := make([]*Layer, len(layers))
	copy(sorted, layers)
	sort.SliceStable(sorted, func(i, j int) bool {
		ki := layerSortKey(order, sorted[i].Name)
		kj := layerSortKey(order, sorted[j].Name)
		if ki != kj {
			return ki > kj // descending: higher position = lower priority = first
		}
		return sorted[i].ID < sorted[j].ID // tiebreak: older layer loses
	})
	layers = sorted

	palette, renameMaps := mergePalettes(master, layers)

	// Sweep-line event-based composition.
	type event struct {
		pos      int
		layerIdx int // higher = higher priority
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
			// Translate name via rename map; fall through to as-is if not mapped.
			name := r.Name
			if rm != nil {
				if renamed, ok := rm[r.Name]; ok {
					name = renamed
				}
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
			return events[i].isEnd // ends before starts at same position
		}
		return events[i].layerIdx < events[j].layerIdx
	})

	active := make(map[int]string) // layerIdx → active run name
	bestName := func() string {
		best, bestLayer := "", -1
		for lay, name := range active {
			if name != "" && lay > bestLayer {
				best, bestLayer = name, lay
			}
		}
		return best
	}

	var result []StyleRun
	curName, curPos := "", 0

	for i := 0; i < len(events); {
		pos := events[i].pos
		if pos > curPos && curName != "" {
			result = append(result, StyleRun{curName, curPos, pos})
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

// ---- partial flush (addr mode) ----

// parseStyleContent parses a complete style buffer (palette + run lines) into
// its component parts.  Used by partialStyleFlush.
func parseStyleContent(content string) ([]PaletteEntry, []StyleRun) {
	var palette []PaletteEntry
	var runs []StyleRun
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, ":") {
			if e, ok := parsePaletteLine(line[1:]); ok {
				palette = append(palette, e)
			}
		} else {
			if r, ok := parseRunLine(line); ok {
				runs = append(runs, r)
			}
		}
	}
	return palette, runs
}

// partialStyleFlush applies an addr-relative write to a single layer then
// recomposes and flushes all layers to acme.
//
// The bytes in wbuf were written to the layer's style fid with addr [q0, q1)
// active.  Run offsets in wbuf are therefore relative to q0; this function
// converts them to absolute, removes any existing runs for this layer that
// overlap [q0, q1), inserts the new runs in their place, and merges any
// palette entries by name (last writer wins for a given name).
func (w *WinState) partialStyleFlush(layID int, wbuf []byte, q0, q1 int, masterPal []PaletteEntry, order []string) {
	newPalette, relRuns := parseStyleContent(string(wbuf))

	// Convert relative offsets → absolute; clip to [q0, q1).
	absRuns := make([]StyleRun, 0, len(relRuns))
	for _, r := range relRuns {
		abs := StyleRun{r.Name, r.Start + q0, r.End + q0}
		if abs.Start < q0 {
			abs.Start = q0
		}
		if abs.End > q1 {
			abs.End = q1
		}
		if abs.Start < abs.End {
			absRuns = append(absRuns, abs)
		}
	}

	w.mu.Lock()
	for _, l := range w.layers {
		if l.ID != layID {
			continue
		}
		// Remove existing runs that overlap [q0, q1).
		kept := l.Runs[:0]
		for _, r := range l.Runs {
			if r.End <= q0 || r.Start >= q1 {
				kept = append(kept, r)
			}
		}
		l.Runs = append(kept, absRuns...)
		sort.Slice(l.Runs, func(i, j int) bool {
			return l.Runs[i].Start < l.Runs[j].Start
		})
		// Merge palette entries by name (new entry wins on conflict).
		for _, e := range newPalette {
			merged := false
			for i, oe := range l.Palette {
				if oe.Name == e.Name {
					l.Palette[i] = e
					merged = true
					break
				}
			}
			if !merged {
				l.Palette = append(l.Palette, e)
			}
		}
		break
	}
	w.mu.Unlock()

	w.flushLayer(masterPal, order)
}

// ---- diff ----

// palettesEqual reports whether two palettes have the same named entries
// with identical visual definitions (order-insensitive).
func palettesEqual(a, b []PaletteEntry) bool {
	if len(a) != len(b) {
		return false
	}
	bm := make(map[string]PaletteEntry, len(b))
	for _, e := range b {
		bm[e.Name] = e
	}
	for _, e := range a {
		be, ok := bm[e.Name]
		if !ok || !paletteIdentical(e, be) {
			return false
		}
	}
	return true
}

// diffRuns finds the minimal dirty interval between two sorted, non-overlapping
// run slices using a forward/backward scan (Gosling-style redisplay).
//
// The forward scan advances matching runs from the front; the backward scan
// removes matching runs from the back.  The dirty interval is the union of the
// file-space extents of the remaining (changed) spans on each side.
//
// Returns changed=false when the sequences are identical.
func diffRuns(old, new []StyleRun) (q0, q1 int, changed bool) {
	// Forward scan: skip identical prefix.
	i, j := 0, 0
	for i < len(old) && j < len(new) && old[i] == new[j] {
		i++
		j++
	}
	if i == len(old) && j == len(new) {
		return 0, 0, false // identical
	}

	// Backward scan: skip identical suffix.
	ei, ej := len(old)-1, len(new)-1
	for ei >= i && ej >= j && old[ei] == new[ej] {
		ei--
		ej--
	}

	// Dirty interval: union of the changed spans' file extents.
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
		// Changed spans have no file-space extent (shouldn't happen with valid
		// non-empty runs, but fall back to full write for safety).
		return 0, 0, false
	}
	return q0, q1, true
}

// ---- flush ----

// flushLayer composes all layers and sends the result to the per-window
// flusher goroutine, which batches rapid calls into a single write to acme.
func (w *WinState) flushLayer(masterPal []PaletteEntry, order []string) {
	w.mu.Lock()
	snapshot := append([]*Layer(nil), w.layers...)
	w.mu.Unlock()

	newPalette, newRuns := compose(masterPal, order, snapshot)

	select {
	case w.flushCh <- flushMsg{newPalette, newRuns}:
	case <-w.ctx.Done():
	}
}

// flusher is the per-window goroutine that coalesces rapid flushLayer calls.
// ctx is a child of the server root context, cancelled by delWin when the
// window closes or by the root when the process shuts down.
func (w *WinState) flusher(ctx context.Context) {
	timer := time.NewTimer(coalesceDelay)
	timer.Stop()

	var pending *flushMsg

	for {
		select {
		case msg := <-w.flushCh:
			pending = &msg
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(coalesceDelay)

		case <-timer.C:
			if pending == nil {
				continue
			}
			w.mu.Lock()
			oldPal := w.prevPalette
			oldRuns := w.prevRuns
			w.mu.Unlock()

			w.diffAndWrite(oldPal, pending.pal, oldRuns, pending.runs)

			w.mu.Lock()
			w.prevPalette = pending.pal
			w.prevRuns = pending.runs
			w.mu.Unlock()

			pending = nil

		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// diffAndWrite compares the old and new composition results and issues the
// minimal write to acme via Win:
//
//   - If the palette changed, a full replacement is required.
//   - If only runs changed, diffRuns finds the dirty interval and a partial
//     addr-scoped write is used.
//   - If nothing changed, no write is issued.
func (w *WinState) diffAndWrite(oldPal, newPal []PaletteEntry, oldRuns, newRuns []StyleRun) {
	if w.win == nil {
		return
	}
	if palettesEqual(oldPal, newPal) {
		q0, q1, changed := diffRuns(oldRuns, newRuns)
		if !changed {
			return
		}
		if err := w.win.Addr("#%d,#%d", q0, q1); err != nil {
			// addr failed; fall back to full write with full data.
			if err := w.win.Style([]byte(formatStyleFull(newPal, newRuns))); err != nil {
				log.Printf("win %d: full style write: %v", w.ID, err)
			}
			return
		}
		if err := w.win.Style([]byte(formatStyleAt(newPal, newRuns, q0, q1))); err != nil {
			log.Printf("win %d: partial style write: %v", w.ID, err)
		}
		return
	}
	if err := w.win.Style([]byte(formatStyleFull(newPal, newRuns))); err != nil {
		log.Printf("win %d: full style write: %v", w.ID, err)
	}
}
