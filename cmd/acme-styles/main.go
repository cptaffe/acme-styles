package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"9fans.net/go/acme"
	"9fans.net/go/plan9/client"
	"github.com/cptaffe/acme-styles/logger"
	"go.uber.org/zap"
)

func main() {
	stylesFile := flag.String("styles", "", "master palette file (new :name format)")
	srv := flag.String("srv", "", "service path (default: $NAMESPACE/acme-styles)")
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

	// client.Namespace() returns /srv on Plan 9 and the $NAMESPACE dir
	// on other systems, so this default is correct on both.
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

	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
	defer stop()
	ctx = logger.NewContext(ctx, l)

	s := newServer(cfg, ctx)
	go watchLog(s)

	conn, cleanup, err := listen(srvPath)
	if err != nil {
		l.Fatal("listen", zap.String("path", srvPath), zap.Error(err))
	}
	go func() {
		<-ctx.Done()
		cleanup()
	}()

	l.Info("serving", zap.String("path", srvPath))
	s.buildSrv9p().Serve(conn, conn)

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
func watchLog(s *Server) {
	l := logger.L(s.ctx)

	wins, err := acme.Windows()
	if err != nil {
		l.Fatal("acme.Windows", zap.Error(err))
	}
	for _, w := range wins {
		s.addWin(w.ID)
	}

	lr, err := acme.Log()
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


