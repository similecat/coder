package tools

import (
	"path"
	"runtime"
	"strings"
)

// MatchGlob matches a slash-separated or OS-separated relative path against a glob pattern.
// Supports '*', '?', '[...]', '**' (0 or more path segments), and '{a,b}' alternation.
// Patterns without '/' match against the path's base filename (e.g. "*.cpp" matches "src/main.cpp").
// Matching is case-insensitive on Windows.
func MatchGlob(pattern, relPath string) bool {
	pattern = normalizeGlobSlash(pattern)
	relPath = normalizeGlobSlash(relPath)
	if pattern == "" {
		return true
	}
	if relPath == "" || relPath == "." {
		return pattern == "." || pattern == "*" || pattern == "**"
	}

	if runtime.GOOS == "windows" {
		pattern = strings.ToLower(pattern)
		relPath = strings.ToLower(relPath)
	}

	for _, alt := range expandBraces(pattern) {
		alt = normalizeGlobSlash(alt)
		if alt == "" {
			continue
		}
		if !strings.Contains(alt, "/") {
			base := relPath
			if idx := strings.LastIndexByte(relPath, '/'); idx >= 0 {
				base = relPath[idx+1:]
			}
			if ok, err := path.Match(alt, base); err == nil && ok {
				return true
			}
			continue
		}

		patSegs := splitNonEmptySlash(alt)
		pathSegs := splitNonEmptySlash(relPath)
		if matchSegments(patSegs, pathSegs) {
			return true
		}
	}
	return false
}

func normalizeGlobSlash(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, `\`, `/`))
	for strings.HasPrefix(s, "./") {
		s = s[2:]
	}
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimSuffix(s, "/")
	return s
}

func splitNonEmptySlash(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, "/")
	out := raw[:0]
	for _, seg := range raw {
		if seg != "" && seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

func matchSegments(pat, target []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse consecutive "**" segments.
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				// Trailing "**" matches everything remaining.
				return true
			}
			for i := 0; i <= len(target); i++ {
				if matchSegments(pat[1:], target[i:]) {
					return true
				}
			}
			return false
		}
		if len(target) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], target[0])
		if err != nil || !ok {
			return false
		}
		pat = pat[1:]
		target = target[1:]
	}
	return len(target) == 0
}

// expandBraces expands a single or nested '{a,b,c}' alternation group into flat patterns.
func expandBraces(pattern string) []string {
	openIdx := -1
	depth := 0
	closeIdx := -1
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			if depth == 0 {
				openIdx = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 {
					closeIdx = i
					goto found
				}
			}
		}
	}
found:
	if openIdx < 0 || closeIdx < 0 {
		return []string{pattern}
	}

	prefix := pattern[:openIdx]
	body := pattern[openIdx+1 : closeIdx]
	suffix := pattern[closeIdx+1:]

	parts := splitTopLevelComma(body)
	if len(parts) <= 1 {
		return []string{pattern}
	}

	var out []string
	for _, p := range parts {
		out = append(out, expandBraces(prefix+p+suffix)...)
	}
	return out
}

func splitTopLevelComma(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}
