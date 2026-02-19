package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

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

// watchLog watches acme/log and adds/removes window state as windows open/close.
// It mounts acme fresh on each attempt so it retries correctly when started
// before acme — the 9fans.net/go/acme package uses sync.Once and would cache
// the first connection failure forever.
func watchLog(s *Server) {
	for {
		if err := watchLogOnce(s); err != nil {
			log.Printf("acme: %v; retrying in 2s", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func watchLogOnce(s *Server) error {
	fs, err := client.MountService("acme")
	if err != nil {
		return err
	}
	defer fs.Close()

	// Seed with already-open windows by reading the index.
	idx, err := fs.Open("index", plan9.OREAD)
	if err != nil {
		return fmt.Errorf("open index: %v", err)
	}
	sc := bufio.NewScanner(idx)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		s.addWin(id)
	}
	idx.Close()

	// Stream future opens/closes from the log.
	logf, err := fs.Open("log", plan9.OREAD)
	if err != nil {
		return fmt.Errorf("open log: %v", err)
	}
	defer logf.Close()

	buf := make([]byte, 8192)
	for {
		n, err := logf.Read(buf)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read log: %v", err)
		}
		f := strings.SplitN(strings.TrimSpace(string(buf[:n])), " ", 3)
		if len(f) < 2 {
			continue
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		switch f[1] {
		case "new":
			s.addWin(id)
		case "del":
			s.delWin(id)
		}
	}
}
