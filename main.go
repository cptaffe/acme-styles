package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

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

// watchLog watches acme/log and adds/removes window state as windows open/close.
// It retries indefinitely so that acme-styles can be started before acme.
func watchLog(s *Server) {
	for {
		// Seed with already-open windows.
		wins, err := acme.Windows()
		if err != nil {
			log.Printf("acme.Windows: %v; retrying in 2s", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, w := range wins {
			s.addWin(w.ID)
		}

		// Stream future opens/closes from the log.
		lr, err := acme.Log()
		if err != nil {
			log.Printf("acme.Log: %v; retrying in 2s", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for {
			ev, err := lr.Read()
			if err != nil {
				log.Printf("acme log: %v", err)
				break
			}
			switch ev.Op {
			case "new":
				s.addWin(ev.ID)
			case "del":
				s.delWin(ev.ID)
			}
		}
		lr.Close()
		time.Sleep(2 * time.Second)
	}
}
