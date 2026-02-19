package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"9fans.net/go/acme"
	"9fans.net/go/plan9"
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

// watchWindow opens <winid>/log in the acme namespace and tracks body
// insertions ("I q0 n") and deletions ("D q0 q1") to keep all layer entry
// positions correct between LSP token updates.  The file is read-only and
// requires no write-back, so we are never a blocking participant in any
// event pipeline.
func watchWindow(s *Server, id int) {
	fs, err := client.MountService("acme")
	if err != nil {
		return
	}
	defer fs.Close()

	fid, err := fs.Open(fmt.Sprintf("%d/log", id), plan9.OREAD)
	if err != nil {
		return // window not found or acme too old
	}
	defer fid.Close()

	sc := bufio.NewScanner(fid)
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 5 {
			continue
		}
		op := line[0]
		if op != 'I' && op != 'D' {
			continue
		}
		var a, b int
		if n, err := fmt.Sscanf(line[2:], "%d %d", &a, &b); err != nil || n != 2 {
			continue
		}
		ws := s.getWin(id)
		if ws == nil {
			continue
		}
		switch op {
		case 'I':
			ws.insertAt(a, b)
		case 'D':
			ws.deleteRange(a, b)
		}
	}
}
