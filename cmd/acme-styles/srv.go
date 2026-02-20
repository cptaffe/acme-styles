package main

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"9fans.net/go/plan9"
	"github.com/cptaffe/acme-styles/logger"
	"go.uber.org/zap"
)

// File-type constants; encode directly into Qid.Path.
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
)

func isDir(ft int) bool {
	return ft == ftRoot || ft == ftWinDir || ft == ftLayersDir || ft == ftLayerDir
}

// Qid path encoding: [ft:16][winID:24][layID:24]
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
		// Accept any numeric window ID.  WinStates are created by watchLog;
		// operations that need the state (NewLayer, etc.) check getWin and
		// return "window gone" if it has not been registered yet.
		return ftWinDir, id, 0, nil
	case ftWinDir:
		if name == "layers" {
			return ftLayersDir, winID, 0, nil
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

// readDir returns marshalled plan9.Dir entries for the children of ft.
func (s *Server) readDir(ft, winID, layID int) []byte {
	var dirs []plan9.Dir
	switch ft {
	case ftRoot:
		for _, id := range s.winIDs() {
			dirs = append(dirs, s.makeDir(ftWinDir, id, 0))
		}
	case ftWinDir:
		dirs = append(dirs, s.makeDir(ftLayersDir, winID, 0))
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

// ---- per-connection state ----

// winLayKey identifies a (window, layer) pair for pending addr tracking.
type winLayKey struct{ winID, layID int }

// addrRange is a pending addr written to ftAddr, consumed by the next
// ftStyle OWRITE on the same (winID, layID).
type addrRange struct{ q0, q1 int }

type fid struct {
	ft    int
	winID int
	layID int
	open  bool
	mode  uint8
	buf   []byte // buffered read content (set at Topen)
	wbuf  []byte // accumulated write bytes (flushed at Tclunk)
	// addr for partial style writes; captured from conn.pendingAddr when this
	// ftStyle fid is opened OWRITE.
	hasAddr bool
	addrQ0  int
	addrQ1  int
}

type conn struct {
	srv         *Server
	fids        map[uint32]*fid
	msize       uint32
	pendingAddr map[winLayKey]addrRange // set by ftAddr write; consumed by ftStyle open
}

func (s *Server) handleConn(c net.Conn) {
	defer c.Close()
	log := logger.L(s.ctx)
	cn := &conn{
		srv:         s,
		fids:        make(map[uint32]*fid),
		msize:       8192 + plan9.IOHDRSZ,
		pendingAddr: make(map[winLayKey]addrRange),
	}
	for {
		fc, err := plan9.ReadFcall(c)
		if err != nil {
			return
		}
		start := time.Now()
		resp := cn.dispatch(fc)
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			log.Warn("slow dispatch",
				zap.String("type", fcallTypeName(fc.Type)),
				zap.Duration("elapsed", elapsed))
		}
		if err := plan9.WriteFcall(c, resp); err != nil {
			return
		}
	}
}

func rerr(tag uint16, msg string) *plan9.Fcall {
	return &plan9.Fcall{Type: plan9.Rerror, Tag: tag, Ename: msg}
}

func (cn *conn) dispatch(fc *plan9.Fcall) *plan9.Fcall {
	switch fc.Type {
	case plan9.Tversion:
		return cn.doVersion(fc)
	case plan9.Tauth:
		return rerr(fc.Tag, "no authentication required")
	case plan9.Tattach:
		return cn.doAttach(fc)
	case plan9.Tflush:
		return &plan9.Fcall{Type: plan9.Rflush, Tag: fc.Tag}
	case plan9.Twalk:
		return cn.doWalk(fc)
	case plan9.Topen:
		return cn.doOpen(fc)
	case plan9.Tcreate:
		return rerr(fc.Tag, "create not supported")
	case plan9.Tread:
		return cn.doRead(fc)
	case plan9.Twrite:
		return cn.doWrite(fc)
	case plan9.Tclunk:
		return cn.doClunk(fc)
	case plan9.Tremove:
		return rerr(fc.Tag, "remove not supported")
	case plan9.Tstat:
		return cn.doStat(fc)
	case plan9.Twstat:
		return rerr(fc.Tag, "wstat not supported")
	default:
		return rerr(fc.Tag, "unknown message type")
	}
}

func (cn *conn) doVersion(fc *plan9.Fcall) *plan9.Fcall {
	msize := fc.Msize
	if msize > cn.msize {
		msize = cn.msize
	}
	cn.msize = msize
	cn.fids = make(map[uint32]*fid)
	ver := "9P2000"
	if !strings.HasPrefix(fc.Version, "9P2000") {
		ver = "unknown"
	}
	return &plan9.Fcall{Type: plan9.Rversion, Tag: fc.Tag, Msize: msize, Version: ver}
}

func (cn *conn) doAttach(fc *plan9.Fcall) *plan9.Fcall {
	cn.fids[fc.Fid] = &fid{ft: ftRoot}
	return &plan9.Fcall{
		Type: plan9.Rattach,
		Tag:  fc.Tag,
		Qid:  cn.srv.makeQID(ftRoot, 0, 0),
	}
}

func (cn *conn) doWalk(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	if f.open {
		return rerr(fc.Tag, "fid is open")
	}

	curFt, curWin, curLay := f.ft, f.winID, f.layID
	wqids := make([]plan9.Qid, 0, len(fc.Wname))

	for i, name := range fc.Wname {
		nft, nwin, nlay, err := cn.srv.walkStep(curFt, curWin, curLay, name)
		if err != nil {
			if i == 0 {
				return rerr(fc.Tag, err.Error())
			}
			break
		}
		wqids = append(wqids, cn.srv.makeQID(nft, nwin, nlay))
		curFt, curWin, curLay = nft, nwin, nlay
	}

	newf := &fid{ft: curFt, winID: curWin, layID: curLay}
	if len(fc.Wname) == 0 {
		newf.ft, newf.winID, newf.layID = f.ft, f.winID, f.layID
	}

	if len(wqids) == len(fc.Wname) {
		cn.fids[fc.Newfid] = newf
	}

	return &plan9.Fcall{Type: plan9.Rwalk, Tag: fc.Tag, Wqid: wqids}
}

func (cn *conn) doOpen(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	if f.open {
		return rerr(fc.Tag, "already open")
	}
	s := cn.srv
	mode := fc.Mode & 3

	switch f.ft {
	case ftRoot, ftWinDir, ftLayersDir, ftLayerDir:
		if mode != plan9.OREAD {
			return rerr(fc.Tag, "is a directory")
		}
		f.buf = s.readDir(f.ft, f.winID, f.layID)

	case ftNew:
		if mode != plan9.OREAD {
			return rerr(fc.Tag, "permission denied")
		}
		w := s.getWin(f.winID)
		if w == nil {
			return rerr(fc.Tag, "window gone")
		}
		id, err := w.NewLayer()
		if err != nil {
			return rerr(fc.Tag, err.Error())
		}
		f.layID = id
		f.buf = []byte(strconv.Itoa(id) + "\n")

	case ftIndex:
		if mode != plan9.OREAD {
			return rerr(fc.Tag, "permission denied")
		}
		w := s.getWin(f.winID)
		if w == nil {
			return rerr(fc.Tag, "window gone")
		}
		f.buf = []byte(w.IndexText())

	case ftName:
		if mode == plan9.OREAD || mode == plan9.ORDWR {
			w := s.getWin(f.winID)
			if w != nil {
				f.buf = []byte(w.LayerName(f.layID) + "\n")
			}
		}

	case ftCtl:
		if mode != plan9.OWRITE {
			return rerr(fc.Tag, "permission denied")
		}

	case ftAddr:
		if mode != plan9.OWRITE {
			return rerr(fc.Tag, "permission denied")
		}

	case ftStyle:
		if mode == plan9.OREAD || mode == plan9.ORDWR {
			w := s.getWin(f.winID)
			if w != nil {
				f.buf = []byte(w.StyleText(f.layID))
			}
		}
		if mode == plan9.OWRITE || mode == plan9.ORDWR {
			// Capture any pending addr written to ftAddr on this connection.
			key := winLayKey{f.winID, f.layID}
			if ar, ok := cn.pendingAddr[key]; ok {
				f.hasAddr = true
				f.addrQ0 = ar.q0
				f.addrQ1 = ar.q1
				delete(cn.pendingAddr, key)
			}
		}
	}

	f.open = true
	f.mode = fc.Mode
	return &plan9.Fcall{
		Type:   plan9.Ropen,
		Tag:    fc.Tag,
		Qid:    s.makeQID(f.ft, f.winID, f.layID),
		Iounit: cn.msize - plan9.IOHDRSZ,
	}
}

func (cn *conn) doRead(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	if !f.open {
		return rerr(fc.Tag, "not open")
	}
	off := fc.Offset
	n := fc.Count
	if off >= uint64(len(f.buf)) {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Data: nil}
	}
	end := off + uint64(n)
	if end > uint64(len(f.buf)) {
		end = uint64(len(f.buf))
	}
	return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Data: f.buf[off:end]}
}

func (cn *conn) doWrite(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	if !f.open {
		return rerr(fc.Tag, "not open")
	}
	s := cn.srv
	n := len(fc.Data)

	switch f.ft {
	case ftCtl:
		f.wbuf = append(f.wbuf, fc.Data...)
		for {
			nl := bytes.IndexByte(f.wbuf, '\n')
			if nl < 0 {
				break
			}
			cmd := strings.TrimSpace(string(f.wbuf[:nl]))
			f.wbuf = f.wbuf[nl+1:]
			if cmd == "" {
				continue
			}
			w := s.getWin(f.winID)
			if w == nil {
				return rerr(fc.Tag, "window gone")
			}
			switch cmd {
			case "clear":
				w.ClearLayer(f.layID)
			case "delete":
				w.DelLayer(f.layID)
			default:
				return rerr(fc.Tag, "unknown ctl command: "+cmd)
			}
		}

	case ftAddr:
		f.wbuf = append(f.wbuf, fc.Data...)
		text := strings.TrimSpace(string(f.wbuf))
		var q0, q1 int
		if _, err := fmt.Sscanf(text, "%d %d", &q0, &q1); err == nil {
			cn.pendingAddr[winLayKey{f.winID, f.layID}] = addrRange{q0, q1}
		}

	case ftName:
		w := s.getWin(f.winID)
		if w == nil {
			return rerr(fc.Tag, "window gone")
		}
		name := strings.TrimRight(string(fc.Data), "\n\r ")
		w.SetLayerName(f.layID, name)

	case ftStyle:
		// Accumulate all writes; parse and apply as one unit at clunk.
		f.wbuf = append(f.wbuf, fc.Data...)

	default:
		return rerr(fc.Tag, "not writable")
	}
	return &plan9.Fcall{Type: plan9.Rwrite, Tag: fc.Tag, Count: uint32(n)}
}

func (cn *conn) doClunk(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f != nil && f.open {
		mode := f.mode & 3
		isWrite := mode == plan9.OWRITE || mode == plan9.ORDWR
		if isWrite && f.ft == ftStyle {
			w := cn.srv.getWin(f.winID)
			if w != nil {
				palette, runs := parseStyleContent(string(f.wbuf))
				if f.hasAddr {
					// Convert addr-relative offsets to absolute, clip to [q0, q1).
					absRuns := make([]StyleRun, 0, len(runs))
					for _, r := range runs {
						abs := StyleRun{r.Name, r.Start + f.addrQ0, r.End + f.addrQ0}
						if abs.Start < f.addrQ0 {
							abs.Start = f.addrQ0
						}
						if abs.End > f.addrQ1 {
							abs.End = f.addrQ1
						}
						if abs.Start < abs.End {
							absRuns = append(absRuns, abs)
						}
					}
					w.SpliceLayerStyle(f.layID, palette, absRuns, f.addrQ0, f.addrQ1)
				} else {
					w.SetLayerStyle(f.layID, palette, runs)
				}
			}
		}
	}
	delete(cn.fids, fc.Fid)
	return &plan9.Fcall{Type: plan9.Rclunk, Tag: fc.Tag}
}

func (cn *conn) doStat(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	d := cn.srv.makeDir(f.ft, f.winID, f.layID)
	stat, err := d.Bytes()
	if err != nil {
		return rerr(fc.Tag, err.Error())
	}
	return &plan9.Fcall{Type: plan9.Rstat, Tag: fc.Tag, Stat: stat}
}

func fcallTypeName(t uint8) string {
	switch t {
	case plan9.Tversion:
		return "Tversion"
	case plan9.Tauth:
		return "Tauth"
	case plan9.Tattach:
		return "Tattach"
	case plan9.Tflush:
		return "Tflush"
	case plan9.Twalk:
		return "Twalk"
	case plan9.Topen:
		return "Topen"
	case plan9.Tcreate:
		return "Tcreate"
	case plan9.Tread:
		return "Tread"
	case plan9.Twrite:
		return "Twrite"
	case plan9.Tclunk:
		return "Tclunk"
	case plan9.Tremove:
		return "Tremove"
	case plan9.Tstat:
		return "Tstat"
	case plan9.Twstat:
		return "Twstat"
	default:
		return fmt.Sprintf("T%d", t)
	}
}
