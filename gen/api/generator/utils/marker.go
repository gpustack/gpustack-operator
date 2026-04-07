package utils

import (
	"strings"
)

func ParseMarker(line string) map[string]string {
	rm := map[string]string{}

	h, r, ok := strings.Cut(line, "=")
	for ; ok; h, r, ok = strings.Cut(r, "=") {
		h = strings.TrimSpace(h)
		r = strings.TrimSpace(r)

		if r != "" {
			switch r[0] {
			case '[', '{', '"':
				if i := indexJSON(r); -1 < i && i < len(r)-1 {
					rm[h] = r[:i+1]
					r = r[i+2:]
					continue
				}
			default:
				if i := strings.Index(r, ","); -1 < i && i < len(r) {
					rm[h] = r[:i]
					r = r[i+1:]
					continue
				}
			}
		}

		rm[h] = r
	}

	return rm
}

func indexJSON(r string) int {
	if len(r) == 0 || r[0] == '\\' {
		return -1
	}

	var (
		c  = 1
		qs = r[0] == '"'
		bs bool
	)

	for i := 1; i < len(r); i++ {
		if bs {
			bs = false
			continue
		}

		switch r[i] {
		case '\\':
			bs = true
			continue
		case '"':
			qs = !qs
			if qs {
				continue
			}
			if r[0] == '"' {
				c--
			}
		case '{', '[':
			if !qs {
				c++
				continue
			}
		case '}', ']':
			if qs {
				continue
			}
			c--
		}

		if c == 0 {
			return i
		}
	}

	return -1
}
