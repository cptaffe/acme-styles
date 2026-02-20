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
	defer ln.Close()
	defer os.Remove(srvPath)

	l.Info("listening", zap.String("addr", srvPath))

	for {
		c, err := ln.Accept()
		if err != nil {
			l.Error("accept", zap.Error(err))
			continue
		}
		go s.handleConn(c)
	}
}

// watchLog seeds window state from acme and then streams opens/closes.
// Any failure is fatal — launchd will restart us.
// Each addWin call starts a window goroutine that clears stale styles and
// tracks edits; no extra per-window goroutines are needed here.
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
	for {
		ev, err := lr.Read()
		if err != nil {
			l.Fatal("acme log", zap.Error(err))
		}
		switch ev.Op {
		case "new":
			s.addWin(ev.ID)
		case "del":
			s.delWin(ev.ID)
		}
	}
}
