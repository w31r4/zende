package patch

import "strings"

// Span uses one-based line positions in the original and resulting file. A zero
// count denotes an insertion/deletion; context lines belong to the hunk span.
type Span struct {
	OldStart int `json:"old_start"`
	OldCount int `json:"old_count"`
	NewStart int `json:"new_start"`
	NewCount int `json:"new_count"`
}

type textLine struct {
	text, end string
	added     bool
}

func splitText(text string) []textLine {
	var out []textLine
	for len(text) > 0 {
		at := strings.IndexByte(text, '\n')
		if at < 0 {
			out = append(out, textLine{text: text})
			break
		}
		line, end := text[:at], "\n"
		if strings.HasSuffix(line, "\r") {
			line, end = strings.TrimSuffix(line, "\r"), "\r\n"
		}
		out = append(out, textLine{text: line, end: end})
		text = text[at+1:]
	}
	return out
}

func updateText(text, path string, chunks []chunk) (string, []Span, error) {
	if len(chunks) == 0 {
		return text, nil, nil
	}
	source := splitText(text)
	defaultEnd := "\n"
	for _, line := range source {
		if line.end != "" {
			defaultEnd = line.end
			break
		}
	}
	var out []textLine
	var spans []Span
	cursor := 0
	for _, c := range chunks {
		start := cursor
		if c.anchor != "" {
			at, count := matchLines(source, []string{c.anchor}, start, false)
			if count != 1 {
				return "", nil, matchError(path, c.line, count, "anchor")
			}
			start = at + 1
		}
		var old []string
		for _, line := range c.lines {
			if line[0] != '+' {
				old = append(old, line[1:])
			}
		}
		at := len(source) // A hunk containing only additions appends at EOF.
		if len(old) > 0 {
			var count int
			at, count = matchLines(source, old, start, c.eof)
			if count != 1 {
				return "", nil, matchError(path, c.line, count, "old text/context")
			}
		}
		out = append(out, source[cursor:at]...)
		span := Span{OldStart: at + 1, OldCount: len(old), NewStart: len(out) + 1}
		end := defaultEnd
		if at < len(source) && source[at].end != "" {
			end = source[at].end
		}
		pos := at
		for _, line := range c.lines {
			switch line[0] {
			case ' ':
				out = append(out, source[pos])
				pos++
				span.NewCount++
			case '-':
				if source[pos].end != "" {
					end = source[pos].end
				}
				pos++
			case '+':
				out = append(out, textLine{text: line[1:], end: end, added: true})
				span.NewCount++
			}
		}
		spans = append(spans, span)
		cursor = pos
	}
	out = append(out, source[cursor:]...)
	// Preserve the original EOF style, adding a separator if appending after a
	// formerly unterminated line. Unchanged/context lines keep their exact EOL.
	if len(source) > 0 && source[len(source)-1].end == "" && len(out) > 0 && out[len(out)-1].added {
		out[len(out)-1].end = ""
	}
	var b strings.Builder
	for i, line := range out {
		b.WriteString(line.text)
		if line.end == "" && i < len(out)-1 {
			b.WriteString(defaultEnd)
		} else {
			b.WriteString(line.end)
		}
	}
	return b.String(), spans, nil
}

func matchError(path string, line, count int, what string) error {
	message := what + " not found"
	if count > 1 {
		message = what + " matches multiple locations; include more context or an End of File marker"
	}
	return problem("patch_conflict", path, line, message)
}

// KMP bounds repeated-line inputs to O(source + pattern), instead of repeatedly
// comparing a long hunk at every matching first line. Stop after two matches.
func matchLines(source []textLine, pattern []string, start int, eof bool) (int, int) {
	fail := make([]int, len(pattern))
	for i, j := 1, 0; i < len(pattern); i++ {
		for j > 0 && pattern[i] != pattern[j] {
			j = fail[j-1]
		}
		if pattern[i] == pattern[j] {
			j++
		}
		fail[i] = j
	}
	at, count, j := -1, 0, 0
	for i := start; i < len(source); i++ {
		for j > 0 && source[i].text != pattern[j] {
			j = fail[j-1]
		}
		if source[i].text == pattern[j] {
			j++
		}
		if j == len(pattern) {
			if !eof || i == len(source)-1 {
				at, count = i-len(pattern)+1, count+1
				if count == 2 {
					return at, count
				}
			}
			j = fail[j-1]
		}
	}
	return at, count
}
