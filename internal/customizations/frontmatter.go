package customizations

import (
	"strings"
)

// Frontmatter contains parsed metadata from an optional leading YAML header in Markdown files.
type Frontmatter struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	ApplyTo     string            `json:"apply_to,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
}

// ParseFrontmatter extracts an optional leading YAML frontmatter block (--- ... ---)
// from raw Markdown content and returns the extracted fields plus trimmed Markdown body.
func ParseFrontmatter(raw string) (Frontmatter, string) {
	text := strings.TrimPrefix(raw, "\xef\xbb\xbf")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	if !hasOpeningFence(text) {
		return Frontmatter{}, strings.TrimSpace(text)
	}

	firstNewline := strings.IndexByte(text, '\n')
	if firstNewline < 0 {
		return Frontmatter{}, strings.TrimSpace(text)
	}

	rest := text[firstNewline+1:]
	lines := strings.Split(rest, "\n")
	endIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "---" || trimmed == "..." {
			endIdx = i
			break
		}
	}
	if endIdx < 0 {
		return Frontmatter{}, strings.TrimSpace(text)
	}

	fmLines := lines[:endIdx]
	if !looksLikeYAMLFrontmatter(fmLines) {
		return Frontmatter{}, strings.TrimSpace(text)
	}

	fm := parseFrontmatterLines(fmLines)
	body := strings.Join(lines[endIdx+1:], "\n")
	return fm, strings.TrimSpace(body)
}

func hasOpeningFence(text string) bool {
	if !strings.HasPrefix(text, "---") {
		return false
	}
	idx := strings.IndexByte(text, '\n')
	if idx < 0 {
		return false
	}
	header := strings.TrimRight(text[:idx], " \t")
	return header == "---"
}

func looksLikeYAMLFrontmatter(lines []string) bool {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if !strings.Contains(line, ":") {
			return false
		}
	}
	return true
}

func parseFrontmatterLines(lines []string) Frontmatter {
	fields := make(map[string]string)

	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			i++
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			i++
			continue
		}

		colonIdx := strings.IndexByte(line, ':')
		if colonIdx <= 0 {
			i++
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		rawVal := strings.TrimSpace(line[colonIdx+1:])
		i++

		// Block scalar (> / | / >- / |- / >+ / |+)
		if isBlockScalarIndicator(rawVal) {
			folded := strings.HasPrefix(rawVal, ">")
			var blockLines []string
			for i < len(lines) {
				next := lines[i]
				if strings.TrimSpace(next) == "" {
					blockLines = append(blockLines, "")
					i++
					continue
				}
				if next[0] != ' ' && next[0] != '\t' {
					break
				}
				blockLines = append(blockLines, strings.TrimSpace(next))
				i++
			}
			fields[key] = joinBlockScalar(blockLines, folded)
			continue
		}

		// Empty value followed by indented YAML list items (- item) or indented lines
		if rawVal == "" {
			var seqItems []string
			var textLines []string
			for i < len(lines) {
				next := lines[i]
				nextTrim := strings.TrimSpace(next)
				if nextTrim == "" {
					i++
					continue
				}
				if next[0] != ' ' && next[0] != '\t' {
					break
				}
				if strings.HasPrefix(nextTrim, "- ") || nextTrim == "-" {
					item := strings.TrimSpace(strings.TrimPrefix(nextTrim, "-"))
					item = parseScalarValue(item)
					if item != "" {
						seqItems = append(seqItems, item)
					}
				} else {
					textLines = append(textLines, nextTrim)
				}
				i++
			}
			if len(seqItems) > 0 {
				fields[key] = strings.Join(seqItems, ", ")
			} else if len(textLines) > 0 {
				fields[key] = strings.Join(textLines, " ")
			} else {
				fields[key] = ""
			}
			continue
		}

		// Inline list [a, b, c]
		if strings.HasPrefix(rawVal, "[") {
			clean := stripUnquotedYAMLComment(rawVal)
			if strings.HasSuffix(clean, "]") {
				items := parseInlineYAMLList(clean[1 : len(clean)-1])
				fields[key] = strings.Join(items, ", ")
				continue
			}
		}

		fields[key] = parseScalarValue(rawVal)
	}

	fm := Frontmatter{}
	if len(fields) > 0 {
		fm.Fields = fields
	}
	for k, v := range fields {
		switch strings.ToLower(k) {
		case "name":
			fm.Name = v
		case "description":
			fm.Description = v
		case "applyto", "apply_to":
			fm.ApplyTo = v
		case "globs":
			if fm.ApplyTo == "" {
				fm.ApplyTo = v
			}
		}
	}
	return fm
}

func isBlockScalarIndicator(val string) bool {
	switch val {
	case ">", ">-", ">+", "|", "|-", "|+":
		return true
	default:
		return false
	}
}

func joinBlockScalar(lines []string, folded bool) string {
	// Trim leading/trailing empty lines
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	if !folded {
		return strings.Join(lines, "\n")
	}
	var parts []string
	for _, l := range lines {
		if l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " ")
}

func parseInlineYAMLList(inner string) []string {
	var items []string
	var cur strings.Builder
	var quote byte
	escaped := false

	flush := func() {
		val := parseScalarValue(strings.TrimSpace(cur.String()))
		if val != "" {
			items = append(items, val)
		}
		cur.Reset()
	}

	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if quote == '"' && ch == '\\' {
			cur.WriteByte(ch)
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				if quote == '\'' && i+1 < len(inner) && inner[i+1] == '\'' {
					cur.WriteByte('\'')
					cur.WriteByte('\'')
					i++
					continue
				}
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
		if ch == ',' {
			flush()
			continue
		}
		cur.WriteByte(ch)
	}
	flush()
	return items
}

func parseScalarValue(raw string) string {
	s := stripUnquotedYAMLComment(strings.TrimSpace(raw))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return unescapeDoubleQuoted(s[1 : len(s)-1])
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

func stripUnquotedYAMLComment(s string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '#' && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

func unescapeDoubleQuoted(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
