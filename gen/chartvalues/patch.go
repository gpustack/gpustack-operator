package main

import (
	"fmt"
	"strings"
)

// Block is a named, generated chunk of YAML text meant to be embedded between a
// pair of marker comments in a target file. Content is written as if its first
// line started at column 0; Patch re-indents every line to the begin marker's own
// indentation, so a Block never needs to know where it will be nested.
type Block struct {
	// Name identifies the block. It appears in its marker comments as
	// "# gpustack:chartvalues:<name>:begin" / "# gpustack:chartvalues:<name>:end".
	Name string
	// Content is the block's generated YAML text.
	Content string
}

// beginMarker and endMarker are the comment lines (ignoring surrounding
// whitespace) that delimit a block of the given name in a target file.
func beginMarker(name string) string { return "# gpustack:chartvalues:" + name + ":begin" }
func endMarker(name string) string   { return "# gpustack:chartvalues:" + name + ":end" }

// Patch replaces the text between every begin/end marker pair it finds in src for
// each block with that block's Content, re-indented to match the begin marker's
// own leading whitespace. A block whose markers are absent from src is left
// untouched, so a target file can adopt the markers one block at a time. A marker
// name may occur more than once in src (e.g. once under a "kueue:" values block
// and once under "kueue-legacy:"); every occurrence is kept in sync. It returns an
// error if a begin marker has no matching end marker.
func Patch(src []byte, blocks []Block) ([]byte, error) {
	lines := strings.Split(string(src), "\n")
	for _, b := range blocks {
		var err error
		lines, err = patchBlock(lines, b)
		if err != nil {
			return nil, fmt.Errorf("block %q: %w", b.Name, err)
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// patchBlock replaces every begin/end occurrence of a single block in lines.
func patchBlock(lines []string, b Block) ([]string, error) {
	begin, end := beginMarker(b.Name), endMarker(b.Name)
	content := strings.Split(strings.TrimRight(b.Content, "\n"), "\n")

	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) != begin {
			out = append(out, line)
			continue
		}

		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		out = append(out, line)

		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) != end {
			j++
		}
		if j >= len(lines) {
			return nil, fmt.Errorf("%q has no matching %q", begin, end)
		}

		for _, cl := range content {
			if cl == "" {
				out = append(out, "")
				continue
			}
			out = append(out, indent+cl)
		}
		out = append(out, lines[j])
		i = j
	}
	return out, nil
}
