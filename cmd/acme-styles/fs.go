package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"9fans.net/go/plan9"
	"9fans.net/go/plan9/srv9p"
)

// Sentinel walk errors.
var (
	ErrNoFile = errors.New("no such file")
	ErrNotDir = errors.New("not a directory")
)

// File-type constants encoded into Qid.Path.
const (
	ftRoot      = 0
	ftWinDir    = 1
	ftLayersDir = 2
	ftNew       = 3
	ftIndex     = 4
	ftLayerDir  = 5
	ftName      = 6
	ftCtl       = 7
	ftStyle     = 8
	ftAddr      = 9
	ftComposed  = 10 // <winid>/style — last compositor output written to acme
)

func isDir(ft int) bool {
	return ft == ftRoot || ft == ftWinDir || ft == ftLayersDir || ft == ftLayerDir
}

// makePath encodes (ft, winID, layID) into a Qid.Path: [ft:16][winID:24][layID:24].
func makePath(ft, winID, layID int) uint64 {
	return (uint64(ft) << 48) | (uint64(winID) << 24) | uint64(layID)
}

func (s *Server) makeQID(ft, winID, layID int) plan9.Qid {
	qt := uint8(plan9.QTFILE)
	if isDir(ft) {
		qt = plan9.QTDIR
	}
	return plan9.Qid{Type: qt, Path: makePath(ft, winID, layID)}
}

func (s *Server) makeDir(ft, winID, layID int) plan9.Dir {
	now := uint32(time.Now().Unix())
	var name string
	var mode plan9.Perm
	if isDir(ft) {
		mode = plan9.DMDIR | 0555
	}
	switch ft {
	case ftRoot:
		name = "/"
	case ftWinDir:
		name = strconv.Itoa(winID)
	case ftLayersDir:
		name = "layers"
	case ftNew:
		name = "new"
		mode = 0444
	case ftIndex:
		name = "index"
		mode = 0444
	case ftLayerDir:
		name = strconv.Itoa(layID)
	case ftName:
		name = "name"
		mode = 0666
	case ftCtl:
		name = "ctl"
		mode = 0222
	case ftStyle:
		name = "style"
		mode = 0666
	case ftAddr:
		name = "addr"
		mode = 0222
	case ftComposed:
		name = "style"
		mode = 0444
	}
	return plan9.Dir{
		Qid:   s.makeQID(ft, winID, layID),
		Mode:  mode,
		Atime: now, Mtime: now,
		Name: name,
		Uid:  "none", Gid: "none", Muid: "none",
	}
}

// walkStep advances one path component from (ft, winID, layID).
func (s *Server) walkStep(ft, winID, layID int, name string) (int, int, int, error) {
	if name == ".." {
		switch ft {
		case ftRoot:
			return ftRoot, 0, 0, nil
		case ftWinDir:
			return ftRoot, 0, 0, nil
		case ftLayersDir:
			return ftWinDir, winID, 0, nil
		case ftLayerDir:
			return ftLayersDir, winID, 0, nil
		default:
			return 0, 0, 0, ErrNotDir
		}
	}
	switch ft {
	case ftRoot:
		id, err := strconv.Atoi(name)
		if err != nil {
			return 0, 0, 0, ErrNoFile
		}
		return ftWinDir, id, 0, nil
	case ftWinDir:
		switch name {
		case "layers":
			return ftLayersDir, winID, 0, nil
		case "style":
			return ftComposed, winID, 0, nil
		}
		return 0, 0, 0, ErrNoFile
	case ftLayersDir:
		switch name {
		case "new":
			return ftNew, winID, 0, nil
		case "index":
			return ftIndex, winID, 0, nil
		}
		id, err := strconv.Atoi(name)
		if err != nil {
			return 0, 0, 0, ErrNoFile
		}
		w := s.getWin(winID)
		if w == nil || !w.LayerExists(id) {
			return 0, 0, 0, ErrNoFile
		}
		return ftLayerDir, winID, id, nil
	case ftLayerDir:
		switch name {
		case "name":
			return ftName, winID, layID, nil
		case "ctl":
			return ftCtl, winID, layID, nil
		case "style":
			return ftStyle, winID, layID, nil
		case "addr":
			return ftAddr, winID, layID, nil
		}
		return 0, 0, 0, ErrNoFile
	default:
		return 0, 0, 0, ErrNotDir
	}
}

// buildReadDir returns pre-marshalled plan9.Dir entries for a directory fid.
func (s *Server) buildReadDir(ft, winID, layID int) []byte {
	var dirs []plan9.Dir
	switch ft {
	case ftRoot:
		for _, id := range s.winIDs() {
			dirs = append(dirs, s.makeDir(ftWinDir, id, 0))
		}
	case ftWinDir:
		dirs = append(dirs, s.makeDir(ftLayersDir, winID, 0))
		dirs = append(dirs, s.makeDir(ftComposed, winID, 0))
	case ftLayersDir:
		dirs = append(dirs, s.makeDir(ftNew, winID, 0))
		dirs = append(dirs, s.makeDir(ftIndex, winID, 0))
		if w := s.getWin(winID); w != nil {
			for _, id := range w.LayerIDs() {
				dirs = append(dirs, s.makeDir(ftLayerDir, winID, id))
			}
		}
	case ftLayerDir:
		dirs = append(dirs, s.makeDir(ftName, winID, layID))
		dirs = append(dirs, s.makeDir(ftCtl, winID, layID))
		dirs = append(dirs, s.makeDir(ftStyle, winID, layID))
		dirs = append(dirs, s.makeDir(ftAddr, winID, layID))
	}
	var buf []byte
	for _, d := range dirs {
		if b, err := d.Bytes(); err == nil {
			buf = append(buf, b...)
		}
	}
	return buf
}

// fidAux holds per-fid application state, stored in fid.Aux().
type fidAux struct {
	ft    int
	winID int
	layID int
	rbuf  []byte // pre-built read content (filled at Open, consumed by Read)
	wbuf  []byte // accumulated write data (applied at Clunk)
	// hasAddr/addrQ0/addrQ1 are captured from the layer at style Open time,
	// matching acme's per-fid stylehasaddr/styleaddr fields.
	hasAddr bool
	addrQ0  int
	addrQ1  int
}

// buildSrv9p constructs the srv9p.Server whose callbacks implement the
// acme-styles virtual filesystem.
func (s *Server) buildSrv9p() *srv9p.Server {
	return &srv9p.Server{
		Attach: func(ctx context.Context, fid, _ *srv9p.Fid, _, _ string) (plan9.Qid, error) {
			qid := s.makeQID(ftRoot, 0, 0)
			fid.SetAux(&fidAux{ft: ftRoot})
			fid.SetQid(qid)
			return qid, nil
		},
		Walk:  s.srvWalk,
		Open:  s.srvOpen,
		Read:  s.srvRead,
		Write: s.srvWrite,
		Stat: func(ctx context.Context, fid *srv9p.Fid) (*plan9.Dir, error) {
			a := fid.Aux().(*fidAux)
			d := s.makeDir(a.ft, a.winID, a.layID)
			return &d, nil
		},
		Clunk: s.srvClunk,
	}
}

func (s *Server) srvWalk(ctx context.Context, fid, newfid *srv9p.Fid, names []string) ([]plan9.Qid, error) {
	a := fid.Aux().(*fidAux)

	// Clone: copy position to newfid and return empty qids.
	if len(names) == 0 {
		newA := *a
		newfid.SetAux(&newA)
		return nil, nil
	}

	curFt, curWin, curLay := a.ft, a.winID, a.layID
	var qids []plan9.Qid
	for i, name := range names {
		nft, nwin, nlay, err := s.walkStep(curFt, curWin, curLay, name)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			break
		}
		qids = append(qids, s.makeQID(nft, nwin, nlay))
		curFt, curWin, curLay = nft, nwin, nlay
	}

	// Point newfid at the last successfully reached position.
	newfid.SetAux(&fidAux{ft: curFt, winID: curWin, layID: curLay})
	return qids, nil
}

func (s *Server) srvOpen(ctx context.Context, fid *srv9p.Fid, mode uint8) error {
	a := fid.Aux().(*fidAux)
	m := mode & 3

	switch a.ft {
	case ftRoot, ftWinDir, ftLayersDir, ftLayerDir:
		// Directories are read-only; eagerly build their listing.
		a.rbuf = s.buildReadDir(a.ft, a.winID, a.layID)

	case ftNew:
		if m != plan9.OREAD {
			return fmt.Errorf("permission denied")
		}
		w := s.getWin(a.winID)
		if w == nil {
			return fmt.Errorf("window gone")
		}
		id, err := w.NewLayer()
		if err != nil {
			return err
		}
		a.layID = id
		fid.SetQid(s.makeQID(ftNew, a.winID, id))
		a.rbuf = []byte(strconv.Itoa(id) + "\n")

	case ftIndex:
		if m != plan9.OREAD {
			return fmt.Errorf("permission denied")
		}
		w := s.getWin(a.winID)
		if w == nil {
			return fmt.Errorf("window gone")
		}
		a.rbuf = []byte(w.IndexText())

	case ftName:
		if m == plan9.OREAD || m == plan9.ORDWR {
			if w := s.getWin(a.winID); w != nil {
				a.rbuf = []byte(w.LayerName(a.layID) + "\n")
			}
		}

	case ftCtl:
		if m != plan9.OWRITE {
			return fmt.Errorf("permission denied")
		}

	case ftAddr:
		if m != plan9.OWRITE {
			return fmt.Errorf("permission denied")
		}
		// Matches acme xfid.c QWaddr Topen: reset pending addr on first open
		// (analogous to w->addr = range(0,0) when nopen[QWaddr]++ == 0).
		if w := s.getWin(a.winID); w != nil {
			w.ResetAddr(a.layID)
		}

	case ftComposed:
		if m != plan9.OREAD {
			return fmt.Errorf("permission denied")
		}
		if w := s.getWin(a.winID); w != nil {
			a.rbuf = []byte(w.ComposedText())
		}

	case ftStyle:
		if m == plan9.OREAD || m == plan9.ORDWR {
			if w := s.getWin(a.winID); w != nil {
				a.rbuf = []byte(w.StyleText(a.layID))
			}
		}
		if m == plan9.OWRITE || m == plan9.ORDWR {
			// Matches acme xfid.c QWstyle Topen: capture and clear the layer's
			// pending addr into the fid (analogous to copying w->hasaddr/w->addr
			// into x->f->stylehasaddr/x->f->styleaddr and clearing w->hasaddr).
			if w := s.getWin(a.winID); w != nil {
				a.addrQ0, a.addrQ1, a.hasAddr = w.ConsumeAddr(a.layID)
			}
		}
	}

	return nil
}

func (s *Server) srvRead(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
	a := fid.Aux().(*fidAux)
	return fid.ReadBytes(data, offset, a.rbuf)
}

func (s *Server) srvWrite(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
	a := fid.Aux().(*fidAux)
	n := len(data)

	switch a.ft {
	case ftCtl:
		a.wbuf = append(a.wbuf, data...)
		for {
			nl := bytes.IndexByte(a.wbuf, '\n')
			if nl < 0 {
				break
			}
			cmd := strings.TrimSpace(string(a.wbuf[:nl]))
			a.wbuf = a.wbuf[nl+1:]
			if cmd == "" {
				continue
			}
			w := s.getWin(a.winID)
			if w == nil {
				return 0, fmt.Errorf("window gone")
			}
			switch cmd {
			case "clear":
				w.ClearLayer(a.layID)
			case "delete":
				w.DelLayer(a.layID)
			default:
				return 0, fmt.Errorf("unknown ctl command: %s", cmd)
			}
		}

	case ftAddr:
		a.wbuf = append(a.wbuf, data...)
		// Parse eagerly on each write, as acme does (w->hasaddr updated on Twrite).
		var q0, q1 int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(a.wbuf)), "%d %d", &q0, &q1); err == nil {
			if w := s.getWin(a.winID); w != nil {
				w.SetAddr(a.layID, q0, q1)
			}
		}

	case ftName:
		w := s.getWin(a.winID)
		if w == nil {
			return 0, fmt.Errorf("window gone")
		}
		w.SetLayerName(a.layID, strings.TrimRight(string(data), "\n\r "))

	case ftStyle:
		// Accumulate all writes; parse and apply atomically at Clunk.
		a.wbuf = append(a.wbuf, data...)

	default:
		return 0, fmt.Errorf("not writable")
	}
	return n, nil
}

func (s *Server) srvClunk(fid *srv9p.Fid) {
	a, ok := fid.Aux().(*fidAux)
	if !ok || a == nil || a.ft != ftStyle || len(a.wbuf) == 0 {
		return
	}
	w := s.getWin(a.winID)
	if w == nil {
		return
	}
	palette, runs := parseStyleContent(string(a.wbuf))
	if a.hasAddr {
		absRuns := make([]StyleRun, 0, len(runs))
		for _, r := range runs {
			abs := StyleRun{
				Name:  r.Name,
				Start: r.Start + a.addrQ0,
				End:   r.End + a.addrQ0,
			}
			if abs.Start < a.addrQ0 {
				abs.Start = a.addrQ0
			}
			if abs.End > a.addrQ1 {
				abs.End = a.addrQ1
			}
			if abs.Start < abs.End {
				absRuns = append(absRuns, abs)
			}
		}
		w.SpliceLayerStyle(a.layID, palette, absRuns, a.addrQ0, a.addrQ1)
	} else {
		w.SetLayerStyle(a.layID, palette, runs)
	}
}
