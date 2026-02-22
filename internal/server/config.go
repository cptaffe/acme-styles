package server

import (
	"strconv"
	"strings"

	"github.com/cptaffe/acme-styles/style"
)

// Config holds all values parsed from the styles file.
type Config struct {
	// Palette is the master set of named style definitions prepended to
	// every composite write sent to acme.
	Palette []style.PaletteEntry

	// LayerOrder lists layer names from highest priority (index 0) to
	// lowest.  A "*" entry is the wildcard slot for layers not explicitly
	// named; omitting "*" places unnamed layers at highest priority.
	LayerOrder []string
}

// ParseConfig parses the master styles file into a Config.
func ParseConfig(content string) Config {
	var cfg Config
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
			if e, ok := parsePaletteLine(line[1:]); ok {
				cfg.Palette = append(cfg.Palette, e)
			}
		case strings.HasPrefix(line, "@"):
			if name := strings.TrimSpace(line[1:]); name != "" {
				cfg.LayerOrder = append(cfg.LayerOrder, name)
			}
		}
	}
	return cfg
}

// ParseStyleContent parses a complete style buffer (palette + run lines).
func ParseStyleContent(content string) ([]style.PaletteEntry, []style.StyleRun) {
	var palette []style.PaletteEntry
	var runs []style.StyleRun
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, ":") {
			if e, ok := parsePaletteLine(line[1:]); ok {
				palette = append(palette, e)
			}
		} else {
			if r, ok := parseRunLine(line); ok {
				runs = append(runs, r)
			}
		}
	}
	return palette, runs
}

// parsePaletteLine parses "name [prop ...]" (after the leading ':' is stripped).
func parsePaletteLine(line string) (style.PaletteEntry, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return style.PaletteEntry{}, false
	}
	e := style.PaletteEntry{Name: fields[0]}
	for _, tok := range fields[1:] {
		switch {
		case tok == "bold":
			e.Bold = true
		case tok == "italic":
			e.Italic = true
		case tok == "underline":
			e.Underline = true
		case strings.HasPrefix(tok, "font="):
			e.FontName = tok[5:]
		case strings.HasPrefix(tok, "fg="):
			e.FG = tok[3:]
		case strings.HasPrefix(tok, "bg="):
			e.BG = tok[3:]
		}
	}
	return e, true
}

// parseRunLine parses "start length name".
func parseRunLine(line string) (style.StyleRun, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return style.StyleRun{}, false
	}
	start, err := strconv.Atoi(fields[0])
	if err != nil {
		return style.StyleRun{}, false
	}
	length, err := strconv.Atoi(fields[1])
	if err != nil || length <= 0 {
		return style.StyleRun{}, false
	}
	return style.StyleRun{Name: fields[2], Start: start, End: start + length}, true
}
