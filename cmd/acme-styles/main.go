package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"9fans.net/go/acme"
	"9fans.net/go/plan9/client"
	"github.com/cptaffe/acme-styles/logger"
	"go.uber.org/zap"
)

// Sentinel walk errors.
var (
	ErrNoFile = errors.New("no such file")
	ErrNotDir = errors.New("not a directory")
)

func main() {
	stylesFile := flag.String("styles", "", "master palette file (new :name format)")
	srv := flag.String("srv", "", "unix socket path (default: $NAMESPACE/acme-styles)")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	var err error
	var l *zap.Logger
	if *verbose {
		l, err = zap.NewDevelopment()
	} else {
		l, err = zap.NewProduction()
	}
	if err != nil {
		log.Fatalf("init logger: %v", err)
	}
	zap.ReplaceGlobals(l)
	defer l.Sync() //nolint:errcheck

	srvPath := *srv
	if srvPath == "" {
		srvPath = client.Namespace() + "/acme-styles"
	}

	var cfg Config
	if *stylesFile != "" {
		data, err := os.ReadFile(*stylesFile)
		if err != nil {
			l.Fatal("read styles", zap.String("path", *stylesFile), zap.Error(err))
		}
		cfg = ParseConfig(string(data))
		l.Info("loaded styles",
			zap.Int("palette", len(cfg.Palette)),
			zap.Int("layerOrder", len(cfg.LayerOrder)),
			zap.String("path", *stylesFile))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = logger.NewContext(ctx, l)

	s := newServer(cfg, ctx)
	go watchLog(s)

	os.Remove(srvPath)
	ln, err := net.Listen("unix", srvPath)
	if err != nil {
		l.Fatal("listen", zap.String("path", srvPath), zap.Error(err))
	}
	defer os.Remove(srvPath)
	defer ln.Close()

	// Close the listener when the context is cancelled so that Accept
	// returns an error and the loop below can exit.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	l.Info("listening", zap.String("addr", srvPath))

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			l.Error("accept", zap.Error(err))
			continue
		}
		go s.handleConn(c)
	}

	l.Info("shutting down; waiting for window goroutines")
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		l.Info("shutdown complete")
	case <-time.After(5 * time.Second):
		l.Warn("shutdown timed out; exiting anyway")
	}
}

// watchLog seeds window state from acme and then streams opens/closes.
// Each addWin call starts a window goroutine that clears stale styles and
// tracks edits; no extra per-window goroutines are needed here.
//
// acme.Windows() and acme.Log() are retried with backoff: when the previous
// process exits abruptly, the 9P proxy (9pserve) may still be sending Tclunk
// messages for its outstanding fids, leaving acme's fid table temporarily busy.
// Retrying gives that cleanup time to finish before we give up.
//
// lr.Read() is a blocking call; we wrap each call in a goroutine and select
// against ctx.Done() so that a signal causes a clean exit.  The in-flight
// goroutine may linger briefly after shutdown — that is fine because the
// process is about to exit.
func watchLog(s *Server) {
	l := logger.L(s.ctx)

	wins, err := retryOn(s.ctx, 10, 200*time.Millisecond, acme.Windows)
	if err != nil {
		l.Fatal("acme.Windows", zap.Error(err))
	}
	for _, w := range wins {
		s.addWin(w.ID)
	}

	lr, err := retryOn(s.ctx, 10, 200*time.Millisecond, acme.Log)
	if err != nil {
		l.Fatal("acme.Log", zap.Error(err))
	}
	defer lr.Close()

	type logResult struct {
		ev  acme.LogEvent
		err error
	}
	ch := make(chan logResult, 1)

	readNext := func() {
		go func() {
			ev, err := lr.Read()
			ch <- logResult{ev, err}
		}()
	}
	readNext()

	for {
		select {
		case <-s.ctx.Done():
			return
		case res := <-ch:
			if res.err != nil {
				if s.ctx.Err() != nil {
					return
				}
				l.Fatal("acme log", zap.Error(res.err))
			}
			switch res.ev.Op {
			case "new":
				s.addWin(res.ev.ID)
			case "del":
				s.delWin(res.ev.ID)
			}
			readNext()
		}
	}
}

// retryOn calls fn repeatedly until it succeeds, the context is cancelled,
// or maxAttempts is exhausted.  Between each attempt it waits delay.
func retryOn[T any](ctx context.Context, maxAttempts int, delay time.Duration, fn func() (T, error)) (T, error) {
	var (
		zero T
		err  error
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var v T
		v, err = fn()
		if err == nil {
			return v, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}
	return zero, err
}
