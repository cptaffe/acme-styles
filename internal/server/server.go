package server

import (
	"context"
	"sort"
	"sync"

	"9fans.net/go/acme"
	"github.com/cptaffe/acme-styles/logger"
	"github.com/cptaffe/acme-styles/style"
	"go.uber.org/zap"
)

// Server is the global service state.
//
// masterPalette and layerOrder are read-only after NewServer returns and may
// be accessed from any goroutine without holding mu.
//
// mu protects only the wins map; it is never held while doing any I/O.
type Server struct {
	masterPalette []style.PaletteEntry
	layerOrder    []string
	mu            sync.Mutex
	wins          map[int]*WinState
	ctx           context.Context // root context; cancelled on shutdown
	wg            sync.WaitGroup  // tracks live window goroutines
}

// NewServer constructs a Server from the parsed config and root context.
func NewServer(cfg Config, ctx context.Context) *Server {
	return &Server{
		masterPalette: cfg.Palette,
		layerOrder:    cfg.LayerOrder,
		wins:          make(map[int]*WinState),
		ctx:           ctx,
	}
}

// Ctx returns the root context of the server.
func (s *Server) Ctx() context.Context {
	return s.ctx
}

// Wait blocks until all window goroutines have exited.
func (s *Server) Wait() {
	s.wg.Wait()
}

// AddWin creates a WinState and starts its goroutine for the given window ID.
// Returns nil if the window was already registered.
//
// s.mu is never held while calling acme.Open: the lock is released, the
// (potentially slow) open is performed, then the lock is re-acquired to
// insert.  If another goroutine raced to add the same ID, the loser discards
// what it opened.
func (s *Server) AddWin(id int) *WinState {
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

// DelWin removes the window from the registry and cancels its goroutine.
// The run() goroutine closes the acme.Win when it sees ctx.Done().
func (s *Server) DelWin(id int) {
	s.mu.Lock()
	ws := s.wins[id]
	delete(s.wins, id)
	s.mu.Unlock()
	if ws != nil {
		ws.cancel()
	}
}

// GetWin returns the WinState for the given window ID, or nil if not found.
func (s *Server) GetWin(id int) *WinState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wins[id]
}

// WinIDs returns all registered window IDs in ascending order.
func (s *Server) WinIDs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int, 0, len(s.wins))
	for id := range s.wins {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}
