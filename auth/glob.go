package auth

// Glob reports whether s matches pattern, where `*` is any run of characters,
// slashes included, and `?` is any one character. Nothing else is special:
// repository names and tags have no other characters a pattern would want.
func Glob(pattern, s string) bool {
	// The classic two-pointer match, with the last star to backtrack to.
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
