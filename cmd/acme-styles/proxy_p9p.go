//go:build !plan9

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// shutdownSignals are the OS signals that trigger a clean exit.
// SIGTERM is included for launchd/systemd service managers.
var shutdownSignals = []os.Signal{os.Interrupt, unix.SIGTERM}

// listen removes any stale socket, forks 9pserve announcing at
// unix!srvPath, and returns the server end of the socketpair.
// 9pserve multiplexes client connections onto the pipe; we serve 9P
// on our end.  The returned cleanup function kills 9pserve and closes
// the connection; call it when the context is cancelled.
func listen(srvPath string) (io.ReadWriteCloser, func(), error) {
	os.Remove(srvPath)

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	// macOS exposes SOCK_CLOEXEC in <sys/socket.h> but x/sys/unix does
	// not define it for darwin, so we set FD_CLOEXEC with a separate
	// call.  This ensures both ends are closed at exec: the parent end
	// (fds[0]) vanishes entirely; the child end (fds[1]) vanishes as its
	// original fd number, but the copies that exec.Cmd dup2's onto fd 0
	// and fd 1 survive because dup2 clears FD_CLOEXEC on the destination.
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	parent := os.NewFile(uintptr(fds[0]), "acme-styles-srv")
	child := os.NewFile(uintptr(fds[1]), "acme-styles-9pserve")

	cmd := exec.Command("9pserve", "unix!"+srvPath)
	cmd.Stdin = child
	cmd.Stdout = child
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		parent.Close()
		child.Close()
		return nil, nil, fmt.Errorf("9pserve: %w", err)
	}
	child.Close() // parent closes child end

	cleanup := func() {
		parent.Close() // 9pserve gets EOF on stdin and exits naturally
		cmd.Wait()     //nolint:errcheck
	}
	return parent, cleanup, nil
}
