package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"9fans.net/go/acme"
)

// StyleMap maps style names to their numeric indices.
type StyleMap map[string]int

func loadStyles(path string) (StyleMap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := make(StyleMap)
	sc := bufio.NewScanner(f)
	idx := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// First token is the name; rest is colour data we ignore here.
		m[strings.Fields(line)[0]] = idx
		idx++
	}
	return m, sc.Err()
}

func (sm StyleMap) parseEntry(line string) (StyleEntry, error) {
	f := strings.Fields(line)
	if len(f) != 3 {
		return StyleEntry{}, fmt.Errorf("want 3 fields, got %d", len(f))
	}
	var idx int
	if n, err := strconv.Atoi(f[0]); err == nil {
		idx = n
	} else {
		var ok bool
		idx, ok = sm[f[0]]
		if !ok {
			return StyleEntry{}, fmt.Errorf("unknown style %q", f[0])
		}
	}
	start, err := strconv.Atoi(f[1])
	if err != nil {
		return StyleEntry{}, fmt.Errorf("bad start: %v", err)
	}
	length, err := strconv.Atoi(f[2])
	if err != nil {
		return StyleEntry{}, fmt.Errorf("bad length: %v", err)
	}
	return StyleEntry{Idx: idx, Start: start, Length: length}, nil
}

// StyleEntry is a (style-index, start-position, length) triple.
type StyleEntry struct {
	Idx    int
	Start  int
	Length int
}

// Layer is a named collection of style entries.
type Layer struct {
	ID      int
	Name    string
	Entries []StyleEntry
}

// WinState holds all layers for one acme window.
type WinState struct {
	ID     int
	mu     sync.Mutex
	layers []*Layer // ordered by creation; later index = higher priority
	nextID int
}

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
			var sb strings.Builder
			for _, e := range l.Entries {
				fmt.Fprintf(&sb, "%d %d %d\n", e.Idx, e.Start, e.Length)
			}
			return sb.String()
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

// insertAt shifts all layer entry positions after n characters inserted at q0.
func (ws *WinState) insertAt(q0, n int) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, l := range ws.layers {
		for i := range l.Entries {
			e := &l.Entries[i]
			if q0 <= e.Start {
				e.Start += n
			} else if q0 < e.Start+e.Length {
				e.Length += n
			}
		}
	}
}

// deleteRange shrinks all layer entry positions after characters [q0,q1) are removed.
func (ws *WinState) deleteRange(q0, q1 int) {
	n := q1 - q0
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, l := range ws.layers {
		out := l.Entries[:0]
		for _, e := range l.Entries {
			eEnd := e.Start + e.Length
			if eEnd <= q0 {
				out = append(out, e)
				continue
			}
			if e.Start >= q1 {
				e.Start -= n
				out = append(out, e)
				continue
			}
			// Overlap: retain the parts before q0 and after q1.
			before := 0
			if e.Start < q0 {
				before = q0 - e.Start
			}
			after := 0
			if eEnd > q1 {
				after = eEnd - q1
			}
			if newLen := before + after; newLen > 0 {
				newStart := e.Start
				if newStart > q0 {
					newStart = q0
				}
				out = append(out, StyleEntry{Idx: e.Idx, Start: newStart, Length: newLen})
			}
			// If newLen == 0 the entry is entirely inside the deleted range; discard it.
		}
		l.Entries = out
	}
}

// clearLayer removes all entries from the given layer and recomposes.
func (w *WinState) clearLayer(layID int) {
	w.mu.Lock()
	for _, l := range w.layers {
		if l.ID == layID {
			l.Entries = nil
			break
		}
	}
	snapshot := append([]*Layer(nil), w.layers...)
	w.mu.Unlock()
	w.writeCtl(compose(snapshot))
}

// parseStyleWrite processes accumulated write bytes for a style file.
// Complete lines are parsed and added to the layer; incomplete lines
// remain in *wbuf for the next write.
func (w *WinState) parseStyleWrite(layID int, wbuf *[]byte, sm StyleMap) error {
	var newEntries []StyleEntry
	buf := *wbuf
	for {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(string(buf[:nl]))
		buf = buf[nl+1:]
		if line == "" {
			continue
		}
		e, err := sm.parseEntry(line)
		if err != nil {
			*wbuf = buf
			return err
		}
		newEntries = append(newEntries, e)
	}
	*wbuf = buf
	if len(newEntries) == 0 {
		return nil
	}
	w.mu.Lock()
	for _, l := range w.layers {
		if l.ID == layID {
			l.Entries = append(l.Entries, newEntries...)
			break
		}
	}
	snapshot := append([]*Layer(nil), w.layers...)
	w.mu.Unlock()
	w.writeCtl(compose(snapshot))
	return nil
}

// writeCtl writes the composed style to the acme window's ctl file.
// "style 0" clears acme's single layer; subsequent "style" commands fill it.
func (w *WinState) writeCtl(entries []StyleEntry) {
	win, err := acme.Open(w.ID, nil)
	if err != nil {
		return // window may be gone
	}
	defer win.CloseFiles()

	if err := win.Ctl("style 0"); err != nil {
		return
	}

	// Write in chunks to stay well within the iounit.
	const chunk = 200
	for i := 0; i < len(entries); i += chunk {
		end := i + chunk
		if end > len(entries) {
			end = len(entries)
		}
		var sb strings.Builder
		sb.WriteString("style")
		for _, e := range entries[i:end] {
			fmt.Fprintf(&sb, " %d %d %d", e.Idx, e.Start, e.Length)
		}
		if err := win.Ctl("%s", sb.String()); err != nil {
			return
		}
	}
}

// compose merges all layers into a sorted, non-overlapping slice.
// Higher-index layers win; style 0 is transparent.
func compose(layers []*Layer) []StyleEntry {
	type event struct {
		pos   int
		layer int // index into layers (higher = higher priority)
		style int
		isEnd bool
	}
	var events []event
	for li, l := range layers {
		for _, e := range l.Entries {
			if e.Idx == 0 || e.Length <= 0 {
				continue
			}
			events = append(events, event{e.Start, li, e.Idx, false})
			events = append(events, event{e.Start + e.Length, li, 0, true})
		}
	}
	if len(events) == 0 {
		return nil
	}

	// Sort: by position; at the same position, ends before starts.
	sort.Slice(events, func(i, j int) bool {
		if events[i].pos != events[j].pos {
			return events[i].pos < events[j].pos
		}
		if events[i].isEnd != events[j].isEnd {
			return events[i].isEnd
		}
		return events[i].layer < events[j].layer
	})

	active := make(map[int]int) // layer index → current style
	best := func() int {
		b, bl := 0, -1
		for lay, sty := range active {
			if sty != 0 && lay > bl {
				b, bl = sty, lay
			}
		}
		return b
	}

	var result []StyleEntry
	curStyle, curPos := 0, 0
	i := 0
	for i < len(events) {
		pos := events[i].pos
		if pos > curPos && curStyle != 0 {
			result = append(result, StyleEntry{curStyle, curPos, pos - curPos})
		}
		for i < len(events) && events[i].pos == pos {
			ev := events[i]
			if ev.isEnd {
				delete(active, ev.layer)
			} else {
				active[ev.layer] = ev.style
			}
			i++
		}
		curPos = pos
		curStyle = best()
	}
	return result
}

// Server is the global service state.
type Server struct {
	mu     sync.Mutex
	styles StyleMap
	wins   map[int]*WinState
}

func newServer(styles StyleMap) *Server {
	return &Server{styles: styles, wins: make(map[int]*WinState)}
}

func (s *Server) addWin(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.wins[id]; !ok {
		s.wins[id] = &WinState{ID: id, nextID: 1}
	}
}

func (s *Server) delWin(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.wins, id)
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
