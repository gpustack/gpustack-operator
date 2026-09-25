package modelartifact

import (
	"slices"
	"strings"
)

// FilterEntries keeps the entries an artifact's allow and ignore patterns select, the rule the
// canonical manifest format applies before canonicalization.
//
// The semantics are those of huggingface_hub's filter_repo_objects, so a user can reuse the
// patterns they already pass to that library: a pattern ending in "/" becomes "<pattern>*"; an
// empty allow list keeps every entry; an entry matching any ignore pattern is dropped even when an
// allow pattern matches it. Matching is Python's fnmatch.fnmatchcase, see MatchPattern.
//
// The patterns themselves never enter the digest, so two artifacts whose patterns select the same
// files share one digest, and an artifact with no pattern keeps the digest of the whole commit.
func FilterEntries(entries []ManifestEntry, allow, ignore []string) []ManifestEntry {
	if len(allow) == 0 && len(ignore) == 0 {
		return entries
	}

	allowed, ignored := compilePatterns(allow), compilePatterns(ignore)
	kept := make([]ManifestEntry, 0, len(entries))
	for _, e := range entries {
		if len(allowed) > 0 && !matchAny(allowed, e.Path) {
			continue
		}
		if matchAny(ignored, e.Path) {
			continue
		}
		kept = append(kept, e)
	}

	return kept
}

// MatchPattern reports whether name matches pattern under Python's fnmatch.fnmatchcase.
//
// That is: case-sensitive; "*" matches any sequence and "?" any one character, both crossing "/";
// "[...]" is a set with "a-z" ranges and "[!...]" its complement, a "]" first in the set is a
// member, and a "[" with no closing "]" is a literal. Every other character is literal, backslash
// included. It is a port of CPython's fnmatch.translate rather than a translation into a Go regular
// expression, because a Go character class gives "[:" and "\" meanings Python's does not.
//
// It is fnmatchcase alone: the rule FilterEntries adds, that a pattern ending in "/" stands for
// everything under that directory, is not applied here.
func MatchPattern(pattern, name string) bool {
	return compilePattern(pattern).match([]rune(name))
}

func compilePatterns(patterns []string) []globPattern {
	compiled := make([]globPattern, 0, len(patterns))
	for _, p := range patterns {
		if strings.HasSuffix(p, "/") {
			p += "*"
		}
		compiled = append(compiled, compilePattern(p))
	}

	return compiled
}

func matchAny(patterns []globPattern, name string) bool {
	rs := []rune(name)
	for _, p := range patterns {
		if p.match(rs) {
			return true
		}
	}

	return false
}

type globTokenKind int

const (
	globLiteral globTokenKind = iota
	globAny
	globStar
	globSet
	globNever
)

type globRange struct{ lo, hi rune }

type globToken struct {
	kind    globTokenKind
	literal rune
	negated bool
	ranges  []globRange
}

type globPattern []globToken

// compilePattern follows CPython's fnmatch._translate step for step: the same scan for the closing
// bracket, the same splitting of a set into hyphen-joined chunks, and the same removal of a range
// whose bounds are reversed, which drops both bounds.
func compilePattern(pattern string) globPattern {
	pat := []rune(pattern)
	n := len(pat)

	var toks globPattern
	for i := 0; i < n; {
		c := pat[i]
		i++
		switch c {
		case '*':
			if len(toks) == 0 || toks[len(toks)-1].kind != globStar {
				toks = append(toks, globToken{kind: globStar})
			}
		case '?':
			toks = append(toks, globToken{kind: globAny})
		case '[':
			j := i
			if j < n && pat[j] == '!' {
				j++
			}
			if j < n && pat[j] == ']' {
				j++
			}
			for j < n && pat[j] != ']' {
				j++
			}
			if j >= n {
				toks = append(toks, globToken{kind: globLiteral, literal: '['})
				continue
			}
			toks = append(toks, compileSet(pat, i, j))
			i = j + 1
		default:
			toks = append(toks, globToken{kind: globLiteral, literal: c})
		}
	}

	return toks
}

// compileSet compiles pat[i:j], the text between "[" and its "]".
func compileSet(pat []rune, i, j int) globToken {
	var chunks [][]rune
	if !slices.Contains(pat[i:j], '-') {
		chunks = [][]rune{pat[i:j]}
	} else {
		k := i + 1
		if pat[i] == '!' {
			k = i + 2
		}
		for {
			k = indexRune(pat, '-', k, j)
			if k < 0 {
				break
			}
			chunks = append(chunks, pat[i:k])
			i = k + 1
			k += 3
		}
		if chunk := pat[i:j]; len(chunk) > 0 {
			chunks = append(chunks, chunk)
		} else {
			last := len(chunks) - 1
			chunks[last] = append(append([]rune(nil), chunks[last]...), '-')
		}
		for k := len(chunks) - 1; k > 0; k-- {
			prev, cur := chunks[k-1], chunks[k]
			if prev[len(prev)-1] > cur[0] {
				merged := append(append([]rune(nil), prev[:len(prev)-1]...), cur[1:]...)
				chunks[k-1] = merged
				chunks = append(chunks[:k], chunks[k+1:]...)
			}
		}
	}

	// Rebuild the set's text the way CPython joins the chunks, with each join a range between the
	// last character of one chunk and the first of the next.
	var (
		members []rune
		isRange []bool
	)
	for ci, chunk := range chunks {
		for ri, r := range chunk {
			members = append(members, r)
			isRange = append(isRange, ci > 0 && ri == 0)
		}
	}
	if len(members) == 0 {
		return globToken{kind: globNever}
	}

	tok := globToken{kind: globSet}
	start := 0
	if members[0] == '!' {
		if len(members) == 1 {
			return globToken{kind: globAny}
		}
		tok.negated = true
		start = 1
	}
	for m := start; m < len(members); m++ {
		if m+1 < len(members) && isRange[m+1] && m+1 > start {
			tok.ranges = append(tok.ranges, globRange{lo: members[m], hi: members[m+1]})
			m++
			continue
		}
		tok.ranges = append(tok.ranges, globRange{lo: members[m], hi: members[m]})
	}

	return tok
}

func (t globToken) matchRune(r rune) bool {
	switch t.kind {
	case globLiteral:
		return r == t.literal
	case globAny:
		return true
	case globSet:
		in := false
		for _, rg := range t.ranges {
			if rg.lo <= r && r <= rg.hi {
				in = true
				break
			}
		}
		return in != t.negated
	default:
		return false
	}
}

// match matches the whole of name. A star retries from one rune further on each failure, the usual
// single-backtrack glob walk, which is enough because consecutive stars are merged when compiling.
func (p globPattern) match(name []rune) bool {
	pi, ni := 0, 0
	starPi, starNi := -1, 0
	for ni < len(name) {
		switch {
		case pi < len(p) && p[pi].kind == globStar:
			starPi, starNi = pi, ni
			pi++
		case pi < len(p) && p[pi].matchRune(name[ni]):
			pi++
			ni++
		case starPi >= 0:
			starNi++
			pi, ni = starPi+1, starNi
		default:
			return false
		}
	}
	for pi < len(p) && p[pi].kind == globStar {
		pi++
	}

	return pi == len(p)
}

func indexRune(rs []rune, r rune, from, to int) int {
	for k := from; k < to; k++ {
		if rs[k] == r {
			return k
		}
	}

	return -1
}
