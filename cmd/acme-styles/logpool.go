package main

import (
	"fmt"
	"sync"

	"9fans.net/go/plan9"
	"9fans.net/go/plan9/client"
)

// logPoolMaxLoad is the maximum number of concurrent long-polling Tread calls
// per pooled connection.  9pserve stalls its connthread when there are ≥64
// outstanding messages per connection (MAXMSG=64).  We leave 16 slots of
// headroom for the transient Walk+Open messages needed when opening a new log
// fid, so a single connection never hits the limit even during startup bursts.
const logPoolMaxLoad = 48

// logPool distributes long-polling id/log Tread calls across a set of acme
// connections so that no single connection exceeds MAXMSG.
//
// Creating connections is serialized via a cond: if all existing connections
// are full and no connection is currently being created, one goroutine mounts
// a new connection while the rest wait.  This prevents a 110-window startup
// from spawning 110 concurrent MountService calls.
type logPool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	conns    []*lpConn
	maxLoad  int
	creating bool // a goroutine is currently mounting a new connection
}

type lpConn struct {
	fs   *client.Fsys
	load int // number of outstanding Tread calls on this connection
}

func newLogPool() *logPool {
	p := &logPool{maxLoad: logPoolMaxLoad}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// open picks a pooled connection with available capacity, opens id/log on it,
// and returns the fid plus a release function.  The caller must call release()
// when the fid's Tread is done (i.e. when fid.Close() has been called).
func (p *logPool) open(id int) (*client.Fid, func(), error) {
	fs, release, err := p.acquire()
	if err != nil {
		return nil, nil, err
	}
	fid, err := fs.Open(fmt.Sprintf("%d/log", id), plan9.OREAD)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	return fid, release, nil
}

// acquire returns a connection with a free slot and the matching release func.
func (p *logPool) acquire() (*client.Fsys, func(), error) {
	p.mu.Lock()
	for {
		// Try an existing connection with capacity.
		for _, c := range p.conns {
			if c.load < p.maxLoad {
				c.load++
				p.mu.Unlock()
				return c.fs, p.releaseFn(c), nil
			}
		}
		// All connections full.  If another goroutine is already mounting one,
		// wait for it to finish and retry.
		if p.creating {
			p.cond.Wait()
			continue
		}
		// We are responsible for mounting a new connection.
		p.creating = true
		p.mu.Unlock()

		fs, err := client.MountService("acme")

		p.mu.Lock()
		p.creating = false
		p.cond.Broadcast() // wake goroutines waiting for capacity
		if err != nil {
			p.mu.Unlock()
			return nil, nil, fmt.Errorf("mount acme (log pool): %w", err)
		}
		c := &lpConn{fs: fs, load: 1}
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		return fs, p.releaseFn(c), nil
	}
}

func (p *logPool) releaseFn(c *lpConn) func() {
	return func() {
		p.mu.Lock()
		c.load--
		p.mu.Unlock()
	}
}

// Close closes all pooled connections.  Any goroutines blocked in a Tread call
// on these connections will receive an error and exit.
func (p *logPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.fs.Close() //nolint:errcheck
	}
	p.conns = nil
}
