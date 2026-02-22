// Package layer provides a client-side library for working with the
// acme-styles compositor.
//
// The compositor is a 9P file server that maintains named layers of
// syntax-highlight runs per acme window and composes them into a single
// style stream written to acme's N/style file.
//
// Typical usage for a highlight tool:
//
//	sl, err := layer.Open(winID, "treesitter")
//	if err != nil { ... }
//	defer sl.Delete()           // clean up on exit
//	sl.Apply(entries)           // write highlight spans
//
// Tools that want to supply their own palette definitions alongside runs
// can use Write with raw wire-format text:
//
//	sl.Write(":keyword fg=#569cd6\n10 3 keyword\n")
//
// Or use the style package to construct the wire format from typed values:
//
//	sl.WriteAt(q0, q1, style.Format(palette, runs))
package layer

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"9fans.net/go/plan9"
	"9fans.net/go/plan9/client"
	"github.com/cptaffe/acme-styles/style"
)

// Entry is a contiguous highlight span in an acme window body.
// It is an alias for style.StyleRun; the two types are interchangeable.
type Entry = style.StyleRun

// StyleLayer is a client handle for one named layer in the acme-styles
// compositor.  A single 9P connection is shared across all StyleLayer
// operations in the process and re-established on first use after any
// error.
type StyleLayer struct {
	WinID   int
	LayerID int
	name    string // for re-allocation after compositor restart
}

// ---- connection management ----

var (
	connMu sync.Mutex
	fsys   *client.Fsys
)

// currentFsys returns the cached connection to acme-styles, connecting
// on first use or after a previous connection error has been reset.
func currentFsys() (*client.Fsys, error) {
	connMu.Lock()
	defer connMu.Unlock()
	if fsys != nil {
		return fsys, nil
	}
	fs, err := client.MountService("acme-styles")
	if err != nil {
		return nil, err
	}
	fsys = fs
	return fs, nil
}

// resetFsys clears the cached connection so the next call to currentFsys
// will reconnect.  Call this when any operation returns a connection error.
func resetFsys() {
	connMu.Lock()
	fsys = nil
	connMu.Unlock()
}

// ---- StyleLayer API ----

// Open returns a StyleLayer for the named layer on winID, creating and
// naming it in the compositor if it does not already exist.
// Returns an error only if acme-styles is unreachable.
func Open(winID int, name string) (*StyleLayer, error) {
	fs, err := currentFsys()
	if err != nil {
		return nil, err
	}
	layID, err := FindOrCreate(fs, winID, name)
	if err != nil {
		resetFsys()
		return nil, err
	}
	return &StyleLayer{WinID: winID, LayerID: layID, name: name}, nil
}

// FindLayer returns a StyleLayer for the named layer on winID if it already
// exists in the compositor.  Returns nil, false if the compositor is
// unreachable or no layer with that name exists.
func FindLayer(winID int, name string) (*StyleLayer, bool) {
	fs, err := currentFsys()
	if err != nil {
		return nil, false
	}
	id, ok := Find(fs, winID, name)
	if !ok {
		return nil, false
	}
	return &StyleLayer{WinID: winID, LayerID: id, name: name}, true
}

// Apply writes the given highlight spans to the layer using the master
// palette (defined in ~/lib/acme/styles) for colour/font information.
// Opening the layer's style file OWRITE causes acme-styles to clear and
// replace its contents atomically; a compositor flush fires at fid clunk.
//
// If the layer is gone (compositor restarted), it is re-allocated first.
func (sl *StyleLayer) Apply(entries []Entry) error {
	if sl == nil {
		return nil
	}
	if len(entries) == 0 {
		sl.Clear()
		return nil
	}
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "%d %d %s\n", e.Start, e.End-e.Start, e.Name)
	}
	return sl.Write(sb.String())
}

// Write sends pre-formatted layer text directly to the compositor.
// The format mirrors the acme-styles wire format: optional palette lines
// starting with ':' (e.g. ":keyword fg=#569cd6 bold") followed by run
// lines of the form "start length name".
//
// Use Write instead of Apply when the tool needs to supply its own
// palette definitions rather than relying on the master palette.
//
// If the layer is gone (compositor restarted), it is re-allocated first.
func (sl *StyleLayer) Write(text string) error {
	if sl == nil {
		return nil
	}
	fs, err := currentFsys()
	if err != nil {
		return err
	}

	stylePath := fmt.Sprintf("%d/layers/%d/style", sl.WinID, sl.LayerID)
	fid, err := fs.Open(stylePath, plan9.OWRITE)
	if err != nil {
		resetFsys()
		// Layer gone — compositor restarted.  Re-allocate and retry once.
		fs, err = currentFsys()
		if err != nil {
			return err
		}
		newID, err2 := FindOrCreate(fs, sl.WinID, sl.name)
		if err2 != nil {
			resetFsys()
			return fmt.Errorf("re-alloc layer: %w", err2)
		}
		sl.LayerID = newID
		fid, err = fs.Open(fmt.Sprintf("%d/layers/%d/style", sl.WinID, sl.LayerID), plan9.OWRITE)
		if err != nil {
			resetFsys()
			return err
		}
	}
	defer fid.Close()
	_, err = fid.Write([]byte(text))
	if err != nil {
		resetFsys()
	}
	return err
}

// WriteAt writes text to the layer scoped to the range [q0, q1).  The
// compositor captures the addr at style-open time, so this method writes
// the addr file first and then opens the style file.
//
// text should contain the wire-format content for the range — palette lines
// followed by run lines with offsets relative to q0.  Use style.Format to
// construct text from typed values.
//
// This is the correct API for tools that apply a highlight to a specific
// selection rather than replacing the entire layer.
func (sl *StyleLayer) WriteAt(q0, q1 int, text string) error {
	if sl == nil {
		return nil
	}
	fs, err := currentFsys()
	if err != nil {
		return err
	}

	// Write addr first — the compositor captures it when style is opened.
	addrFid, err := fs.Open(fmt.Sprintf("%d/layers/%d/addr", sl.WinID, sl.LayerID), plan9.OWRITE)
	if err != nil {
		resetFsys()
		return fmt.Errorf("open addr: %w", err)
	}
	_, werr := fmt.Fprintf(addrFid, "%d %d", q0, q1)
	addrFid.Close()
	if werr != nil {
		resetFsys()
		return fmt.Errorf("write addr: %w", werr)
	}

	// Open style — addr is captured at Topen.
	fid, err := fs.Open(fmt.Sprintf("%d/layers/%d/style", sl.WinID, sl.LayerID), plan9.OWRITE)
	if err != nil {
		resetFsys()
		return fmt.Errorf("open style: %w", err)
	}
	_, err = fid.Write([]byte(text))
	fid.Close()
	if err != nil {
		resetFsys()
	}
	return err
}

// Clear sends "clear\n" to the layer's ctl file, removing all runs and
// triggering a compositor flush.  Best-effort: errors are ignored.
func (sl *StyleLayer) Clear() {
	if sl == nil {
		return
	}
	sl.ctl("clear\n")
}

// Delete sends "delete\n" to the layer's ctl file, removing the layer
// entirely from the compositor.  Call this on graceful shutdown so
// highlights don't linger in open windows after the tool exits.
// Best-effort: errors are ignored.
func (sl *StyleLayer) Delete() {
	if sl == nil {
		return
	}
	sl.ctl("delete\n")
}

// ctl writes cmd to the layer's ctl file; best-effort.
func (sl *StyleLayer) ctl(cmd string) {
	fs, err := currentFsys()
	if err != nil {
		return
	}
	fid, err := fs.Open(
		fmt.Sprintf("%d/layers/%d/ctl", sl.WinID, sl.LayerID),
		plan9.OWRITE,
	)
	if err != nil {
		resetFsys()
		return
	}
	if _, err := fid.Write([]byte(cmd)); err != nil {
		resetFsys()
	}
	fid.Close()
}

// ---- functional helpers (for callers that manage their own fs connection) ----

// Find looks up a layer by name in the window's layers/index.
// Returns the layer ID and true if found, 0 and false otherwise.
func Find(fs *client.Fsys, winID int, name string) (int, bool) {
	fid, err := fs.Open(fmt.Sprintf("%d/layers/index", winID), plan9.OREAD)
	if err != nil {
		return 0, false
	}
	data, err := io.ReadAll(fid)
	fid.Close()
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			id, err := strconv.Atoi(fields[0])
			if err == nil {
				return id, true
			}
		}
	}
	return 0, false
}

// FindOrCreate returns the ID of the named layer, creating and naming it
// if it does not already exist.
func FindOrCreate(fs *client.Fsys, winID int, name string) (int, error) {
	if id, ok := Find(fs, winID, name); ok {
		return id, nil
	}

	newFid, err := fs.Open(fmt.Sprintf("%d/layers/new", winID), plan9.OREAD)
	if err != nil {
		return 0, fmt.Errorf("open layers/new: %w", err)
	}
	data, err := io.ReadAll(newFid)
	newFid.Close()
	if err != nil {
		return 0, fmt.Errorf("read layers/new: %w", err)
	}
	layID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse layer id %q: %w", string(data), err)
	}

	nameFid, err := fs.Open(fmt.Sprintf("%d/layers/%d/name", winID, layID), plan9.OWRITE)
	if err != nil {
		return 0, fmt.Errorf("open layer name: %w", err)
	}
	nameFid.Write([]byte(name)) //nolint:errcheck
	nameFid.Close()

	return layID, nil
}

// ReadDot returns the current dot [q0, q1) for the given acme window,
// using the addr file of an already-open acme 9P connection.
// The addr fid is opened before the ctl write so the nopen 0→1 transition
// does not reset the address.
func ReadDot(acmefs *client.Fsys, winID int) (q0, q1 int, err error) {
	addrFid, err := acmefs.Open(fmt.Sprintf("%d/addr", winID), plan9.OREAD)
	if err != nil {
		return 0, 0, fmt.Errorf("open addr: %w", err)
	}
	defer addrFid.Close()

	ctlFid, err := acmefs.Open(fmt.Sprintf("%d/ctl", winID), plan9.OWRITE)
	if err != nil {
		return 0, 0, fmt.Errorf("open ctl: %w", err)
	}
	_, err = ctlFid.Write([]byte("addr=dot"))
	ctlFid.Close()
	if err != nil {
		return 0, 0, fmt.Errorf("write addr=dot: %w", err)
	}

	buf := make([]byte, 40)
	n, _ := addrFid.Read(buf)
	if _, err := fmt.Sscanf(strings.TrimSpace(string(buf[:n])), "%d %d", &q0, &q1); err != nil {
		return 0, 0, fmt.Errorf("parse addr %q: %w", string(buf[:n]), err)
	}
	return q0, q1, nil
}
