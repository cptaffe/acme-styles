//go:build plan9

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"9fans.net/go/plan9/srv9p"
)

// shutdownSignals are the OS signals that trigger a clean exit.
var shutdownSignals = []os.Signal{os.Interrupt}

// listen posts the service to /srv and returns the server end of the pipe.
// srvPath is a full path such as "/srv/acme-styles"; srv9p.Post takes only
// the base name and prepends /srv/ itself.
func listen(srvPath string) (io.ReadWriteCloser, func(), error) {
	rw, err := srv9p.Post(filepath.Base(srvPath))
	if err != nil {
		return nil, nil, fmt.Errorf("post %s: %w", srvPath, err)
	}
	return rw, func() { rw.Close() }, nil
}
