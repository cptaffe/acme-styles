package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"9fans.net/go/acme"
	"9fans.net/go/plan9/client"
)

// Sentinel walk errors.
var (
	ErrNoFile = errors.New("no such file")
	ErrNotDir = errors.New("not a directory")
)

func main() {
	stylesFile := flag.String("styles", "", "styles file mapping names to indices")
	srv := flag.String("srv", "", "unix socket path (default: $NAMESPACE/acme-styles)")
	flag.Parse()

	srvPath := *srv
	if srvPath == "" {
		srvPath = client.Namespace() + "/acme-styles"
	}

	sm := StyleMap{}
	if *stylesFile != "" {
		var err error
		sm, err = loadStyles(*stylesFile)
		if err != nil {
			log.Fatalf("load styles: %v", err)
		}
	}

	s := newServer(sm)
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

// watchLog seeds window state from acme and then streams opens/closes from
// acme/log.  Any failure is fatal — launchd will restart us, providing the
// retry loop and giving acme time to come up.
func watchLog(s *Server) {
	wins, err := acme.Windows()
	if err != nil {
		log.Fatal("acme: ", err)
	}
	for _, w := range wins {
		s.addWin(w.ID)
		go watchWindow(s, w.ID)
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
			s.addWin(ev.ID)
			go watchWindow(s, ev.ID)
		case "del":
			s.delWin(ev.ID)
		}
	}
}

// watchWindow opens the event file for the given acme window and tracks body
// insertions and deletions so that layer entry positions stay correct between
// LSP token updates.  All events are written back so editing proceeds normally.
func watchWindow(s *Server, id int) {
	win, err := acme.Open(id, nil)
	if err != nil {
		return
	}
	defer win.CloseFiles()
	if err := win.OpenEvent(); err != nil {
		return
	}
	for {
		ev, err := win.ReadEvent()
		if err != nil {
			return // window gone or acme exiting
		}
		if ws := s.getWin(id); ws != nil {
			switch ev.C2 {
			case 'I':
				ws.insertAt(ev.Q0, ev.Nr)
			case 'D':
				ws.deleteRange(ev.Q0, ev.Q1)
			}
		}
		win.WriteEvent(ev) //nolint:errcheck
	}
}
