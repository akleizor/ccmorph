package pycompat

// Fnmatch is fnmatch.fnmatchcase: `*`, `?`, `[seq]` and `[!seq]`, case-sensitive.
func Fnmatch(name, pattern string) bool {
	return match([]rune(name), compilePattern([]rune(pattern)))
}

type tok struct {
	kind   byte // 's' star, 'a' any one, 'l' literal, 'c' class, 'n' never
	r      rune
	ranges [][2]rune
	negate bool
}

func compilePattern(p []rune) []tok {
	var out []tok
	for i := 0; i < len(p); {
		c := p[i]
		i++
		switch c {
		case '*':
			if len(out) == 0 || out[len(out)-1].kind != 's' {
				out = append(out, tok{kind: 's'})
			}
		case '?':
			out = append(out, tok{kind: 'a'})
		case '[':
			j := i
			if j < len(p) && p[j] == '!' {
				j++
			}
			if j < len(p) && p[j] == ']' {
				j++
			}
			for j < len(p) && p[j] != ']' {
				j++
			}
			if j >= len(p) {
				out = append(out, tok{kind: 'l', r: '['})
				continue
			}
			stuff := p[i:j]
			i = j + 1
			t := tok{kind: 'c'}
			if len(stuff) > 0 && stuff[0] == '!' {
				t.negate = true
				stuff = stuff[1:]
			}
			if len(stuff) == 0 && !t.negate {
				out = append(out, tok{kind: 'n'}) // "[]" can never match
				continue
			}
			for k := 0; k < len(stuff); k++ {
				if k+2 < len(stuff) && stuff[k+1] == '-' {
					if stuff[k] <= stuff[k+2] {
						t.ranges = append(t.ranges, [2]rune{stuff[k], stuff[k+2]})
					}
					k += 2
					continue
				}
				t.ranges = append(t.ranges, [2]rune{stuff[k], stuff[k]})
			}
			out = append(out, t)
		default:
			out = append(out, tok{kind: 'l', r: c})
		}
	}
	return out
}

func (t tok) accepts(r rune) bool {
	switch t.kind {
	case 'a':
		return true
	case 'l':
		return t.r == r
	case 'c':
		in := false
		for _, rg := range t.ranges {
			if r >= rg[0] && r <= rg[1] {
				in = true
				break
			}
		}
		return in != t.negate
	}
	return false
}

func match(name []rune, toks []tok) bool {
	if len(toks) == 0 {
		return len(name) == 0
	}
	if toks[0].kind == 's' {
		for i := 0; i <= len(name); i++ {
			if match(name[i:], toks[1:]) {
				return true
			}
		}
		return false
	}
	return len(name) > 0 && toks[0].accepts(name[0]) && match(name[1:], toks[1:])
}
