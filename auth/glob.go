package auth

import (
	"fmt"
	"strings"
)

// Glob is a pattern over names made of `/`-separated segments: repositories,
// tags, and the values of a credential's claims.
//
//   - `*` is any run of characters within one segment, none included.
//   - `**`, a segment of its own, is any number of whole segments: none where
//     something follows it, `a/**/b` matching `a/b`, and at least one at the
//     end, so `a/**` is what is under `a` and not `a` itself.
//   - `\` makes the character after it plain: `\*` is a star.
//
// Anything else is itself, and a pattern matches a name whole.
type Glob struct {
	src  string
	segs []segment
}

// segment is one segment of a pattern: the plain runs between its stars, or
// `**`.
type segment struct {
	deep  bool
	parts []string
}

// ParseGlob reads a pattern. A `**` that is not a whole segment, and a `\`
// with nothing after it, are errors.
func ParseGlob(pattern string) (Glob, error) {
	g := Glob{src: pattern}
	for s := range strings.SplitSeq(pattern, "/") {
		if s == "**" {
			if n := len(g.segs); n > 0 && g.segs[n-1].deep {
				continue
			}
			g.segs = append(g.segs, segment{deep: true})
			continue
		}
		var (
			seg segment
			cur strings.Builder
		)
		for i := 0; i < len(s); i++ {
			switch c := s[i]; c {
			case '\\':
				if i+1 == len(s) {
					return Glob{}, fmt.Errorf("glob %q: `\\` with nothing after it", pattern)
				}
				i++
				cur.WriteByte(s[i])
			case '*':
				if i+1 < len(s) && s[i+1] == '*' {
					return Glob{}, fmt.Errorf("glob %q: `**` must be a segment of its own", pattern)
				}
				seg.parts = append(seg.parts, cur.String())
				cur.Reset()
			default:
				cur.WriteByte(c)
			}
		}
		seg.parts = append(seg.parts, cur.String())
		g.segs = append(g.segs, seg)
	}
	return g, nil
}

// MustGlob is ParseGlob for a pattern known to be good.
func MustGlob(pattern string) Glob {
	g, err := ParseGlob(pattern)
	if err != nil {
		panic(err)
	}
	return g
}

func (g Glob) String() string { return g.src }

// Match reports whether s matches the pattern.
func (g Glob) Match(s string) bool {
	return matchSegments(g.segs, strings.Split(s, "/"))
}

func matchSegments(ps []segment, ss []string) bool {
	if len(ps) == 0 {
		return len(ss) == 0
	}
	if ps[0].deep {
		least := 0
		if len(ps) == 1 {
			least = 1
		}
		for i := least; i <= len(ss); i++ {
			if matchSegments(ps[1:], ss[i:]) {
				return true
			}
		}
		return false
	}
	return len(ss) > 0 && ps[0].match(ss[0]) && matchSegments(ps[1:], ss[1:])
}

// match is one segment against its runs: the first at the start, the last at
// the end, and the ones between where they are first found.
func (seg segment) match(s string) bool {
	ps := seg.parts
	if len(ps) == 1 {
		return s == ps[0]
	}
	first, last := ps[0], ps[len(ps)-1]
	if len(s) < len(first)+len(last) || !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return false
	}
	s = s[len(first) : len(s)-len(last)]
	for _, p := range ps[1 : len(ps)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return true
}
