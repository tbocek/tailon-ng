package main

import "strings"

// parseTerms parses a plain-text query into OR-ed groups of AND-ed terms:
// spaces separate terms (OR), a standalone && joins its neighbours (AND),
// "…" quotes a phrase, \" and \\ escape. A dangling &&, an empty "" and an
// unclosed quote are tolerated. frontend/main.js has the same parser; both
// are tested against testdata/terms.json.
func parseTerms(q string) [][]string {
	var groups [][]string
	and := false
	blank := func(c byte) bool { return c == ' ' || c == '\t' }
	for i := 0; i < len(q); {
		if blank(q[i]) {
			i++
			continue
		}
		var tok strings.Builder
		quoted, inQuote := false, false
		for ; i < len(q) && (inQuote || !blank(q[i])); i++ {
			switch c := q[i]; {
			case c == '\\' && i+1 < len(q) && (q[i+1] == '"' || q[i+1] == '\\'):
				i++
				tok.WriteByte(q[i])
			case c == '"':
				inQuote, quoted = !inQuote, true
			default:
				tok.WriteByte(c)
			}
		}
		switch t := tok.String(); {
		case !quoted && t == "&&":
			and = true
		case t != "":
			if n := len(groups); and && n > 0 {
				groups[n-1] = append(groups[n-1], t)
			} else {
				groups = append(groups, []string{t})
			}
			and = false
		}
	}
	return groups
}

// termMatch returns a line test for parsed terms: true if every term of some
// group is contained in the line.
func termMatch(groups [][]string, nocase bool) func(string) bool {
	if nocase {
		for _, g := range groups {
			for i := range g {
				g[i] = strings.ToLower(g[i])
			}
		}
	}
	return func(line string) bool {
		if nocase {
			line = strings.ToLower(line)
		}
	next:
		for _, g := range groups {
			for _, t := range g {
				if !strings.Contains(line, t) {
					continue next
				}
			}
			return true
		}
		return false
	}
}
