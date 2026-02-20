package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"9fans.net/go/acme"
	"9fans.net/go/plan9/client"
)

// Sentinel walk errors.
var (
	ErrNoFile = errors.New("no such file")
	ErrNotDir = errors.New("not a directory")
)

func main() {
	stylesFile := flag.String("styles", "", "master palette file (new :name format)")
	srv := flag.String("srv", "", "unix socket path (default: $NAMESPACE/acme-styles)")
	flag.Parse()

	srvPath := *srv
	if srvPath == "" {
		srvPath = client.Namespace() + "/acme-styles"
	}

	var cfg Config
	if *stylesFile != "" {
		data, err := os.ReadFile(*stylesFile)
		if err != nil {
			log.Fatalf("read styles %s: %v", *stylesFile, err)
		}
		cfg = ParseConfig(string(data))
		log.Printf("acme-styles: loaded %d palette entries, %d layer order entries from %s",
			len(cfg.Palette), len(cfg.LayerOrder), *stylesFile)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := newServer(cfg, ctx)
	go watchLog(s)

	os.Remove(srvPath)
	l, err := net.Listen("unix", srvPath)
	if err != nil {
		log.Fatalf("listen %s: %v", srvPath, err)
	}
	defer l.Close()
	defer os.Remove(srvPath)

	fmt.Fprintln(os.Stderr, "acme-styles: listening on", srvPath)

	for {
		c, err := l.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go s.handleConn(c)
	}
}

// watchLog seeds window state from acme and then streams opens/closes.
// Any failure is fatal — launchd will restart us.
func watchLog(s *Server) {
	wins, err := acme.Windows()
	if err != nil {
		log.Fatal("acme: ", err)
	}
	for _, w := range wins {
		ws := s.addWin(w.ID)
		if ws != nil {
			go ws.clearStyle()
			go watchWindow(ws)
		}
	}

	lr, err := acme.Log()
	if err != nil {
		log.Fatal("acme: ", err)
	}
	for {
		ev, err := lr.Read()
		if err != nil {
			log.Fatal("acme log: ", err)
		}
		switch ev.Op {
		case "new":
			if ws := s.addWin(ev.ID); ws != nil {
				go watchWindow(ws)
			}
		case "del":
			s.delWin(ev.ID)
		}
	}
}

// watchWindow reads ws.win's log channel and tracks body insertions and
// deletions so that all layer run positions stay correct between style updates.
func watchWindow(ws *WinState) {
	if ws.win == nil {
		return
	}
	for e := range ws.win.LogChan() {
		switch e.Op {
		case 'I':
			ws.insertAt(e.Q0, e.Q1)
		case 'D':
			ws.deleteRange(e.Q0, e.Q1)
		}
	}
}
