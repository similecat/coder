package customizations

import (
	"strings"
	"unicode/utf8"
)

const maxBraceExpansions = 64

// IsGlobalApplyTo reports whether an applyTo frontmatter string applies globally to all files.
func IsGlobalApplyTo(applyTo string) bool {
	if strings.TrimSpace(applyTo) == "" {
		return true
	}
	parts := splitCommaPatterns(applyTo)
	if len(parts) == 0 {
		return true
	}
	for _, part := range parts {
		t := normalizeGlobPattern(part)
		if t == "**" || t == "**/*" {
			return true
		}
	}
	return false
}

// MatchApplyTo reports whether any relative path in relPaths matches at least one
// comma-separated glob pattern in applyTo (supporting **, *, ?, [...], and {a,b}).
func MatchApplyTo(applyTo string, relPaths []string) bool {
	rawPatterns := splitCommaPatterns(applyTo)
	if len(rawPatterns) == 0 || len(relPaths) == 0 {
		return false
	}

	var patterns []string
	for _, rp := range rawPatterns {
		p := normalizeGlobPattern(rp)
		if p == "" {
			continue
		}
		expanded := expandBraces(p, maxBraceExpansions)
		for _, ep := range expanded {
			ep = normalizeGlobPattern(ep)
			if ep != "" {
				patterns = append(patterns, ep)
			}
		}
	}
	if len(patterns) == 0 {
		return false
	}

	var normPaths []string
	for _, rp := range relPaths {
		np := normalizeRelPath(rp)
		if np != "" && np != "." {
			normPaths = append(normPaths, np)
		}
	}
	if len(normPaths) == 0 {
		return false
	}

	for _, pat := range patterns {
		if pat == "**" || pat == "**/*" {
			return true
		}
		patSegs := strings.Split(pat, "/")
		for _, np := range normPaths {
			pathSegs := strings.Split(np, "/")
			if matchPathSegments(patSegs, pathSegs) {
				return true
			}
		}
	}
	return false
}

func splitCommaPatterns(applyTo string) []string {
	s := strings.TrimSpace(applyTo)
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	braceDepth := 0
	var quote byte

	flush := func() {
		item := strings.TrimSpace(cur.String())
		if len(item) >= 2 && ((item[0] == '"' && item[len(item)-1] == '"') || (item[0] == '\'' && item[len(item)-1] == '\'')) {
			item = strings.TrimSpace(item[1 : len(item)-1])
		}
		if item != "" {
			out = append(out, item)
		}
		cur.Reset()
	}

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			cur.WriteByte(ch)
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case '{':
			braceDepth++
			cur.WriteByte(ch)
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
			cur.WriteByte(ch)
		case ',':
			if braceDepth == 0 {
				flush()
			} else {
				cur.WriteByte(ch)
			}
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return out
}

func normalizeGlobPattern(p string) string {
	s := strings.TrimSpace(p)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	s = strings.ReplaceAll(s, "\\", "/")
	for strings.HasPrefix(s, "./") {
		s = strings.TrimPrefix(s, "./")
	}
	s = strings.TrimPrefix(s, "/")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimSuffix(s, "/")
	return s
}

func normalizeRelPath(p string) string {
	s := strings.TrimSpace(p)
	s = strings.ReplaceAll(s, "\\", "/")
	for strings.HasPrefix(s, "./") {
		s = strings.TrimPrefix(s, "./")
	}
	s = strings.TrimPrefix(s, "/")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimSuffix(s, "/")
	return s
}

func expandBraces(pattern string, limit int) []string {
	openIdx := -1
	closeIdx := -1
	depth := 0
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '{' {
			if depth == 0 {
				openIdx = i
			}
			depth++
		} else if pattern[i] == '}' && depth > 0 {
			depth--
			if depth == 0 {
				closeIdx = i
				break
			}
		}
	}
	if openIdx < 0 || closeIdx < 0 {
		return []string{pattern}
	}

	prefix := pattern[:openIdx]
	inner := pattern[openIdx+1 : closeIdx]
	suffix := pattern[closeIdx+1:]

	alts := splitTopLevelBraceAlts(inner)
	if len(alts) <= 1 {
		return []string{pattern}
	}

	var results []string
	for _, alt := range alts {
		sub := expandBraces(prefix+alt+suffix, limit-len(results))
		results = append(results, sub...)
		if len(results) >= limit {
			return results[:limit]
		}
	}
	return results
}

func splitTopLevelBraceAlts(inner string) []string {
	var alts []string
	var cur strings.Builder
	depth := 0
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		switch ch {
		case '{':
			depth++
			cur.WriteByte(ch)
		case '}':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(ch)
		case ',':
			if depth == 0 {
				alts = append(alts, cur.String())
				cur.Reset()
			} else {
				cur.WriteByte(ch)
			}
		default:
			cur.WriteByte(ch)
		}
	}
	alts = append(alts, cur.String())
	return alts
}

func matchPathSegments(patSegs, pathSegs []string) bool {
	m := len(patSegs)
	n := len(pathSegs)
	// dp[i][j] reports whether patSegs[i:] matches pathSegs[j:]
	dp := make([][]bool, m+1)
	for i := range dp {
		dp[i] = make([]bool, n+1)
	}
	dp[m][n] = true

	for i := m - 1; i >= 0; i-- {
		if patSegs[i] == "**" {
			dp[i][n] = dp[i+1][n]
			for j := n - 1; j >= 0; j-- {
				dp[i][j] = dp[i+1][j] || dp[i][j+1]
			}
		} else {
			for j := n - 1; j >= 0; j-- {
				if matchSegmentWildcard(patSegs[i], pathSegs[j]) && dp[i+1][j+1] {
					dp[i][j] = true
				}
			}
		}
	}
	return dp[0][0]
}

// matchSegmentWildcard matches a single path segment against a wildcard segment (*, ?, [...]).
func matchSegmentWildcard(pattern, name string) bool {
	px, nx := 0, 0
	nextPx, nextNx := 0, 0

	for px < len(pattern) || nx < len(name) {
		if px < len(pattern) {
			c := pattern[px]
			switch c {
			case '*':
				// Collapse consecutive '*'
				for px+1 < len(pattern) && pattern[px+1] == '*' {
					px++
				}
				nextPx = px
				nextNx = nx + 1
				px++
				continue
			case '?':
				if nx < len(name) {
					_, width := utf8.DecodeRuneInString(name[nx:])
					px++
					nx += width
					continue
				}
			case '[':
				if nx < len(name) {
					r, width := utf8.DecodeRuneInString(name[nx:])
					if matched, newPx, ok := matchCharClass(pattern, px, r); ok {
						if matched {
							px = newPx
							nx += width
							continue
						}
					} else if rune(c) == r {
						px++
						nx += width
						continue
					}
				}
			default:
				if nx < len(name) {
					pr, pw := utf8.DecodeRuneInString(pattern[px:])
					nr, nw := utf8.DecodeRuneInString(name[nx:])
					if pr == nr {
						px += pw
						nx += nw
						continue
					}
				}
			}
		}
		if nextNx > 0 && nextNx <= len(name) {
			// Advance past one full UTF-8 rune from the previous match position
			prev := nextNx - 1
			_, width := utf8.DecodeRuneInString(name[prev:])
			px = nextPx
			nx = prev + width
			nextNx = nx + 1
			continue
		}
		return false
	}
	return true
}

func matchCharClass(pattern string, start int, target rune) (matched bool, nextIdx int, valid bool) {
	i := start + 1
	if i >= len(pattern) {
		return false, 0, false
	}
	negated := false
	if pattern[i] == '!' || pattern[i] == '^' {
		negated = true
		i++
	}
	foundClose := false
	first := true
	hit := false

	for i < len(pattern) {
		if pattern[i] == ']' && !first {
			foundClose = true
			i++
			break
		}
		first = false
		lo, w := utf8.DecodeRuneInString(pattern[i:])
		i += w
		if i+1 < len(pattern) && pattern[i] == '-' && pattern[i+1] != ']' {
			i++ // skip '-'
			hi, hw := utf8.DecodeRuneInString(pattern[i:])
			i += hw
			if lo <= target && target <= hi {
				hit = true
			}
		} else if lo == target {
			hit = true
		}
	}
	if !foundClose {
		return false, 0, false
	}
	if negated {
		hit = !hit
	}
	return hit, i, true
}
