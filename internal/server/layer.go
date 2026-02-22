package server

import "github.com/cptaffe/acme-styles/style"

// Layer is a named collection of palette entries and style runs.
//
// hasAddr/addrQ0/addrQ1 mirror acme's w->hasaddr and w->addr: they are set
// when the layer's addr file is written, cleared when addr is opened
// (nopen 0→1 reset) or consumed at style-open time.
type Layer struct {
	ID      int
	Name    string
	Palette []style.PaletteEntry
	Runs    []style.StyleRun

	hasAddr bool
	addrQ0  int
	addrQ1  int
}
