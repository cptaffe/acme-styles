package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"9fans.net/go/acme"
	"github.com/cptaffe/acme-styles/logger"
	"go.uber.org/zap"
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
// hasAddr/addrQ0/addrQ1 mirror acme's w->hasaddr and w->addr: they are set
// when the layer's addr file is written, cleared when addr is opened
// (nopen 0→1 reset) or consumed at style-open time.
type Layer struct {
	ID      int
	Name    string
	Palette []PaletteEntry
	Runs    []StyleRun

	hasAddr bool
	addrQ0  int
	addrQ1  int
}

// coalesceDelay is the window during which multiple flush triggers are batched
// into a single write to acme.
const coalesceDelay = 20 * time.Millisecond

// flushMsg is a composed palette+run result waiting to be written to acme.
type flushMsg struct {
	pal  []PaletteEntry
	runs []StyleRun
}

// WinState is the actor for one acme window.
//
// The fields ID, ctx, cancel, cmdCh, and srv are set once at construction and
// may be read from any goroutine without a lock.
//
// All remaining fields (win, layers, nextID, prevPalette, prevRuns, pending,
// flushTimer) are owned exclusively by the run() goroutine and must not be
// accessed from any other goroutine.
type WinState struct {
	ID     int
	ctx    context.Context
	cancel context.CancelFunc
	cmdCh  chan func(*WinState)
	srv    *Server

	// Owned by run() — do not access from other goroutines.
	win         *acme.Win
	layers      []*Layer
	nextID      int
	prevPalette []PaletteEntry
	prevRuns    []StyleRun
	pending     *flushMsg
	flushTimer  *time.Timer
	// editCh delivers insert/delete events from id/log.  It is nil until the
	// first layer is created for this window (lazy connection).
	editCh   <-chan *acme.WinLogEvent
	// logChanCh receives the editCh once startLogChan's goroutine completes.
	// It is nil before log tracking starts and nil again once editCh is set.
	logChanCh chan (<-chan *acme.WinLogEvent)
}

// submit enqueues fn to run in the window's goroutine.  Returns immediately;
// fn runs asynchronously.  Drops the fn silently if ctx is already cancelled.
func (ws *WinState) submit(fn func(*WinState)) {
	select {
	case ws.cmdCh <- fn:
	case <-ws.ctx.Done():
	}
}

// callTimeout is the maximum time call() will wait for the window goroutine
// to process a closure.  This guards 9P handlers against an unresponsive
// run() goroutine (e.g. one stuck in a slow 9P call at startup).
const callTimeout = 5 * time.Second

// call enqueues fn and blocks until it has run, ctx is cancelled, or
// callTimeout elapses.  Returns true if fn ran to completion, false if the
// context was cancelled or the timeout elapsed.
func (ws *WinState) call(fn func(*WinState)) bool {
	done := make(chan struct{})
	ws.submit(func(ws *WinState) {
		fn(ws)
		close(done)
	})
	select {
	case <-done:
		return true
	case <-ws.ctx.Done():
		return false
	case <-time.After(callTimeout):
		logger.L(ws.ctx).Warn("call timed out; window goroutine unresponsive")
		return false
	}
}

// run is the window goroutine.  It owns all mutable WinState fields and is
// the only goroutine that touches them.
func (ws *WinState) run() {
	defer ws.srv.wg.Done()
	log := logger.L(ws.ctx)

	ws.flushTimer = time.NewTimer(coalesceDelay)
	ws.flushTimer.Stop()

	// Clear any stale styles from a previous acme-styles run.
	if ws.win != nil {
		log.Debug("clearing stale styles")
		if err := ws.win.Style(nil); err != nil {
			log.Error("clear style", zap.Error(err))
		}
		log.Debug("cleared stale styles")
	}

	// Edit tracking is started lazily: id/log is only opened once the first
	// style layer is created for this window (see maybeStartLog / NewLayer).
	// Windows with no layers never open a log fid.
	log.Debug("entering select loop")

	for {
		select {
		case ch := <-ws.logChanCh:
			ws.editCh = ch
			ws.logChanCh = nil // nil channel blocks forever; remove from select
			log.Debug("opened log chan", zap.Bool("ok", ws.editCh != nil))

		case fn := <-ws.cmdCh:
			fn(ws)

		case e, ok := <-ws.editCh:
			if !ok {
				// Log file closed — window will be deleted shortly via the
				// global acme log.  Stop tracking edits; keep running so that
				// in-flight commands still execute.
				ws.editCh = nil
				continue
			}
			switch e.Op {
			case 'I':
				ws.applyInsert(e.Q0, e.Q1)
			case 'D':
				ws.applyDelete(e.Q0, e.Q1)
			}

		case <-ws.flushTimer.C:
			if ws.pending != nil {
				ws.doFlush()
			}

		case <-ws.ctx.Done():
			ws.flushTimer.Stop()
			if ws.win != nil {
				ws.win.CloseFiles()
				ws.win = nil
			}
			return
		}
	}
}

// startLogChan opens window ws.ID via acme.Open and returns a channel that
// delivers per-window body-edit events.  It is called lazily (via
// maybeStartLog) only when the first layer is created, so windows with no
// active layers never open a log fid.
//
// The goroutine exits when ReadLog returns an error (window deleted or
// process exiting).  run() exits independently on ctx.Done() and does not
// depend on this channel closing first.
func (ws *WinState) startLogChan() <-chan *acme.WinLogEvent {
	log := logger.L(ws.ctx)
	log.Debug("opening log win")
	w, err := acme.Open(ws.ID, nil)
	if err != nil {
		log.Error("open win for log", zap.Error(err))
		return nil
	}
	log.Debug("opened log win")
	ch := make(chan *acme.WinLogEvent)
	go func() {
		defer w.CloseFiles()
		defer close(ch)
		for {
			e, err := w.ReadLog()
			if err != nil {
				return
			}
			ch <- e
		}
	}()
	return ch
}

// ---- internal goroutine-owned helpers ----

func (ws *WinState) applyInsert(q0, n int) {
	for _, l := range ws.layers {
		adjustRunsInsert(l.Runs, q0, n)
	}
	adjustRunsInsert(ws.prevRuns, q0, n)
}

func (ws *WinState) applyDelete(q0, q1 int) {
	for _, l := range ws.layers {
		l.Runs = adjustRunsDelete(l.Runs, q0, q1)
	}
	ws.prevRuns = adjustRunsDelete(ws.prevRuns, q0, q1)
}

// scheduleFlush recomposes all layers and arms the coalesce timer.
// Must be called from within the window goroutine.
func (ws *WinState) scheduleFlush() {
	newPalette, newRuns := compose(ws.srv.masterPalette, ws.srv.layerOrder, ws.layers)
	ws.pending = &flushMsg{newPalette, newRuns}
	resetTimer(ws.flushTimer, coalesceDelay)
}

// doFlush writes the pending composition to acme and clears ws.pending.
// Must be called from within the window goroutine.
func (ws *WinState) doFlush() {
	if ws.pending == nil {
		return
	}
	ws.diffAndWrite(ws.prevPalette, ws.pending.pal, ws.prevRuns, ws.pending.runs)
	ws.prevPalette = ws.pending.pal
	ws.prevRuns = ws.pending.runs
	ws.pending = nil
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// ---- public API for 9P handlers (safe to call from any goroutine) ----

// NewLayer allocates a new layer and returns its ID.
func (ws *WinState) NewLayer() (int, error) {
	var id int
	if !ws.call(func(ws *WinState) {
		l := &Layer{ID: ws.nextID, Name: strconv.Itoa(ws.nextID)}
		ws.layers = append(ws.layers, l)
		id = ws.nextID
		ws.nextID++
		ws.maybeStartLog()
	}) {
		return 0, fmt.Errorf("new layer: call timeout")
	}
	return id, nil
}

// maybeStartLog starts edit tracking for this window if it hasn't been started
// yet.  Must be called from within run() (i.e. inside a call/submit closure).
func (ws *WinState) maybeStartLog() {
	if ws.logChanCh != nil || ws.editCh != nil {
		return // already started or starting
	}
	ws.logChanCh = make(chan (<-chan *acme.WinLogEvent), 1)
	go func() { ws.logChanCh <- ws.startLogChan() }()
}

// LayerExists reports whether a layer with the given ID is present.
func (ws *WinState) LayerExists(id int) bool {
	var exists bool
	ws.call(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == id {
				exists = true
				return
			}
		}
	})
	return exists
}

// LayerIDs returns the IDs of all layers in ascending order.
func (ws *WinState) LayerIDs() []int {
	var ids []int
	ws.call(func(ws *WinState) {
		ids = make([]int, 0, len(ws.layers))
		for _, l := range ws.layers {
			ids = append(ids, l.ID)
		}
		sort.Ints(ids)
	})
	return ids
}

// IndexText returns the content of the layers/index file.
func (ws *WinState) IndexText() string {
	var result string
	ws.call(func(ws *WinState) {
		var sb strings.Builder
		for _, l := range ws.layers {
			fmt.Fprintf(&sb, "%d %s\n", l.ID, l.Name)
		}
		result = sb.String()
	})
	return result
}

// ComposedText returns the last style content that was written to acme —
// i.e. the compositor's most recent full-replacement output for this window.
// Returns an empty string if no flush has happened yet.
func (ws *WinState) ComposedText() string {
	var result string
	ws.call(func(ws *WinState) {
		result = formatStyleFull(ws.prevPalette, ws.prevRuns)
	})
	return result
}

// StyleText returns the serialized palette+runs for one layer.
func (ws *WinState) StyleText(layID int) string {
	var result string
	ws.call(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				result = serializeLayer(l)
				return
			}
		}
	})
	return result
}

// LayerName returns the name of the given layer (empty string if not found).
func (ws *WinState) LayerName(layID int) string {
	var name string
	ws.call(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				name = l.Name
				return
			}
		}
	})
	return name
}

// ResetAddr clears the pending addr for layID, matching acme's behaviour of
// resetting w->addr and w->hasaddr when the addr file is opened for the first
// time (nopen[QWaddr] 0→1).
func (ws *WinState) ResetAddr(layID int) {
	ws.submit(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				l.hasAddr = false
				l.addrQ0, l.addrQ1 = 0, 0
				return
			}
		}
	})
}

// SetAddr records a pending partial-write address for layID, matching acme's
// behaviour of setting w->hasaddr and w->addr on each write to the addr file.
func (ws *WinState) SetAddr(layID int, q0, q1 int) {
	ws.submit(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				l.hasAddr = true
				l.addrQ0 = q0
				l.addrQ1 = q1
				return
			}
		}
	})
}

// ConsumeAddr captures and clears the pending addr for layID, matching acme's
// behaviour of reading w->hasaddr/w->addr into the fid at style Topen time
// and immediately clearing w->hasaddr.  Returns ok=false when no addr is
// pending (full-replace mode).
func (ws *WinState) ConsumeAddr(layID int) (q0, q1 int, ok bool) {
	ws.call(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				q0, q1, ok = l.addrQ0, l.addrQ1, l.hasAddr
				l.hasAddr = false
				l.addrQ0, l.addrQ1 = 0, 0
				return
			}
		}
	})
	return
}

// SetLayerName renames a layer.
func (ws *WinState) SetLayerName(layID int, name string) {
	ws.submit(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				l.Name = name
				return
			}
		}
	})
}

// ClearLayer empties a layer's palette and runs, then schedules a flush.
func (ws *WinState) ClearLayer(layID int) {
	ws.submit(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				l.Palette = nil
				l.Runs = nil
				break
			}
		}
		ws.scheduleFlush()
	})
}

// DelLayer removes a layer entirely, then schedules a flush.
func (ws *WinState) DelLayer(layID int) {
	ws.submit(func(ws *WinState) {
		for i, l := range ws.layers {
			if l.ID == layID {
				ws.layers = append(ws.layers[:i], ws.layers[i+1:]...)
				break
			}
		}
		ws.scheduleFlush()
	})
}

// SetLayerStyle replaces a layer's palette and runs in full, then schedules
// a flush.  Called at clunk of an ftStyle fid opened without an addr.
func (ws *WinState) SetLayerStyle(layID int, palette []PaletteEntry, runs []StyleRun) {
	ws.submit(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				l.Palette = palette
				l.Runs = runs
				break
			}
		}
		ws.scheduleFlush()
	})
}

// SpliceLayerStyle applies a partial update to a layer: removes existing runs
// that overlap [q0, q1), inserts absRuns in their place, merges palette
// entries by name, then schedules a flush.  Called at clunk of an ftStyle fid
// opened with an addr.
func (ws *WinState) SpliceLayerStyle(layID int, palette []PaletteEntry, absRuns []StyleRun, q0, q1 int) {
	ws.submit(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID != layID {
				continue
			}
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
			for _, e := range palette {
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
		ws.scheduleFlush()
	})
}

// ---- Config ----

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

// ---- Server ----

// Server is the global service state.
//
// masterPalette and layerOrder are read-only after newServer returns and may
// be accessed from any goroutine without holding mu.
//
// mu protects only the wins map; it is never held while doing any I/O.
type Server struct {
	masterPalette []PaletteEntry
	layerOrder    []string
	mu            sync.Mutex
	wins map[int]*WinState
	ctx  context.Context // root context; cancelled on shutdown
	wg   sync.WaitGroup  // tracks live window goroutines
}

func newServer(cfg Config, ctx context.Context) *Server {
	return &Server{
		masterPalette: cfg.Palette,
		layerOrder:    cfg.LayerOrder,
		wins:          make(map[int]*WinState),
		ctx:           ctx,
	}
}

// addWin creates a WinState and starts its goroutine for the given window ID.
// Returns nil if the window was already registered.
//
// s.mu is never held while calling acme.Open: the lock is released, the
// (potentially slow) open is performed, then the lock is re-acquired to
// insert.  If another goroutine raced to add the same ID, the loser discards
// what it opened.
func (s *Server) addWin(id int) *WinState {
	s.mu.Lock()
	if _, ok := s.wins[id]; ok {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(s.ctx)
	ctx = logger.NewContext(ctx, logger.L(s.ctx).With(zap.Int("window", id)))

	logger.L(ctx).Debug("opening acme window")
	awin, err := acme.Open(id, nil)
	if err != nil {
		logger.L(ctx).Error("open acme window", zap.Error(err))
		// awin is nil; run() handles nil win gracefully.
	}
	logger.L(ctx).Debug("opened acme window", zap.Bool("ok", awin != nil))

	ws := &WinState{
		ID:     id,
		ctx:    ctx,
		cancel: cancel,
		cmdCh:  make(chan func(*WinState), 64),
		srv:    s,
		win:    awin,
		nextID: 1,
	}

	s.mu.Lock()
	if _, ok := s.wins[id]; ok {
		// Lost the race — another goroutine added this window first.
		s.mu.Unlock()
		cancel()
		if awin != nil {
			awin.CloseFiles()
		}
		return nil
	}
	s.wins[id] = ws
	s.mu.Unlock()

	s.wg.Add(1)
	go ws.run()
	return ws
}

// delWin removes the window from the registry and cancels its goroutine.
// The run() goroutine closes the acme.Win when it sees ctx.Done().
func (s *Server) delWin(id int) {
	s.mu.Lock()
	ws := s.wins[id]
	delete(s.wins, id)
	s.mu.Unlock()
	if ws != nil {
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

// ---- edit tracking (called from run()) ----

// adjustRunsInsert shifts and extends layer runs for an insertion of n runes
// at q0.  Characters inserted strictly inside a run extend it; insertions at
// a boundary fall into the right neighbour.
func adjustRunsInsert(runs []StyleRun, q0, n int) {
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

// parseStyleContent parses a complete style buffer (palette + run lines).
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
				out = append(out, e)
				rm[origName] = origName
			} else if paletteIdentical(out[existingIdx], e) {
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

// layerSortKey returns the sort key for a layer given the order list.
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

// compose merges the master palette with all layers and produces a sorted,
// non-overlapping list of style runs.
func compose(master []PaletteEntry, order []string, layers []*Layer) ([]PaletteEntry, []StyleRun) {
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

// diffRuns finds the minimal dirty interval between two sorted,
// non-overlapping run slices.
func diffRuns(old, new []StyleRun) (q0, q1 int, changed bool) {
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

// ---- flush (called from run()) ----

// diffAndWrite compares old and new compositions and writes the minimal
// diff to acme.  Must be called from within the window goroutine.
func (ws *WinState) diffAndWrite(oldPal, newPal []PaletteEntry, oldRuns, newRuns []StyleRun) {
	if ws.win == nil {
		return
	}
	if palettesEqual(oldPal, newPal) {
		q0, q1, changed := diffRuns(oldRuns, newRuns)
		if !changed {
			return
		}
		if err := ws.win.Addr("#%d,#%d", q0, q1); err != nil {
			if err := ws.win.Style([]byte(formatStyleFull(newPal, newRuns))); err != nil {
				logger.L(ws.ctx).Error("full style write", zap.Error(err))
			}
			return
		}
		if err := ws.win.Style([]byte(formatStyleAt(newPal, newRuns, q0, q1))); err != nil {
			logger.L(ws.ctx).Error("partial style write", zap.Error(err))
		}
		return
	}
	if err := ws.win.Style([]byte(formatStyleFull(newPal, newRuns))); err != nil {
		logger.L(ws.ctx).Error("full style write", zap.Error(err))
	}
}


