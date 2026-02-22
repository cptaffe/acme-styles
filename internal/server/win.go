package server

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"9fans.net/go/acme"
	"github.com/cptaffe/acme-styles/logger"
	"github.com/cptaffe/acme-styles/style"
	"go.uber.org/zap"
)

// coalesceDelay is the window during which multiple flush triggers are batched
// into a single write to acme.
const coalesceDelay = 20 * time.Millisecond

// callTimeout is the maximum time call() will wait for the window goroutine
// to process a closure.  This guards 9P handlers against an unresponsive
// run() goroutine (e.g. one stuck in a slow 9P call at startup).
const callTimeout = 5 * time.Second

// flushMsg is a composed palette+run result waiting to be written to acme.
type flushMsg struct {
	pal  []style.PaletteEntry
	runs []style.StyleRun
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
	prevPalette []style.PaletteEntry
	prevRuns    []style.StyleRun
	pending     *flushMsg
	flushTimer  *time.Timer
	// editCh delivers insert/delete events from id/log.  It is nil until the
	// first layer is created for this window (lazy connection).
	editCh <-chan *acme.WinLogEvent
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

// diffAndWrite compares old and new compositions and writes the minimal
// diff to acme.  Must be called from within the window goroutine.
func (ws *WinState) diffAndWrite(oldPal, newPal []style.PaletteEntry, oldRuns, newRuns []style.StyleRun) {
	if ws.win == nil {
		return
	}
	if palettesEqual(oldPal, newPal) {
		q0, q1, changed := diffRuns(oldRuns, newRuns)
		if !changed {
			return
		}
		if err := ws.win.Addr("#%d,#%d", q0, q1); err != nil {
			if err := ws.win.Style([]byte(style.Format(newPal, newRuns))); err != nil {
				logger.L(ws.ctx).Error("full style write", zap.Error(err))
			}
			return
		}
		if err := ws.win.Style([]byte(formatAt(newPal, newRuns, q0, q1))); err != nil {
			logger.L(ws.ctx).Error("partial style write", zap.Error(err))
		}
		return
	}
	if err := ws.win.Style([]byte(style.Format(newPal, newRuns))); err != nil {
		logger.L(ws.ctx).Error("full style write", zap.Error(err))
	}
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
		result = style.Format(ws.prevPalette, ws.prevRuns)
	})
	return result
}

// StyleText returns the serialized palette+runs for one layer.
func (ws *WinState) StyleText(layID int) string {
	var result string
	ws.call(func(ws *WinState) {
		for _, l := range ws.layers {
			if l.ID == layID {
				result = style.Format(l.Palette, l.Runs)
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
func (ws *WinState) SetLayerStyle(layID int, palette []style.PaletteEntry, runs []style.StyleRun) {
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
func (ws *WinState) SpliceLayerStyle(layID int, palette []style.PaletteEntry, absRuns []style.StyleRun, q0, q1 int) {
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
