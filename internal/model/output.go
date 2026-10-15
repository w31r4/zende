package model

import (
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

// PublicMessage is a display projection, never a replacement for the original
// provider message used in subsequent requests. Opaque data stays on Message.
type PublicMessage struct {
	MessageID string `json:"message_id"`
	Index     int    `json:"index"`
	Category  string `json:"category"`
	Text      string `json:"text"`
}

// PublicDelta is provisional until the whole response is validated. In
// particular an end delta for final cannot finish a Task by itself.
type PublicDelta struct {
	MessageID string `json:"message_id"`
	Index     int    `json:"index"`
	Kind      string `json:"kind"` // start, text, end
	Category  string `json:"category"`
	Text      string `json:"text,omitempty"`
}

type outputSource struct {
	id, category string
	parser       markerParser
	pending      string
	body         strings.Builder
	output       []PublicMessage
	started      bool
	published    int
}

// Classifier accepts only assistant response deltas; user/tool text never enters
// it. A format error is sticky but must not abort collection of raw model data.
type Classifier struct {
	Native  bool
	sources []*outputSource
	byID    map[string]*outputSource
	err     error
	bytes   int
	blocks  int
	text    bool
}

func outputError(message string) error { return protocol.E("output_format", message) }

func (c *Classifier) HasText() bool { return c.text }

func (c *Classifier) Add(d Delta) ([]PublicDelta, error) {
	if d.Kind == TextDelta && d.Text != "" {
		c.text = true
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.byID == nil {
		c.byID = map[string]*outputSource{}
	}
	var changes []PublicDelta
	s := c.byID[d.MessageID]
	switch d.Kind {
	case MessageStart:
		s = &outputSource{id: d.MessageID, category: d.Category}
		c.sources = append(c.sources, s)
		c.byID[d.MessageID] = s
	case TextDelta, MessageEnd, MessageCategory:
		if s == nil {
			return nil, nil
		} // The structural collector diagnoses this.
		c.bytes += len(d.Text)
		if c.bytes > MaxResponseBytes {
			c.err = outputError("visible response exceeds classification budget")
			break
		}
		if c.Native {
			if d.Kind == MessageCategory {
				s.category = d.Category
			}
			if d.Kind == TextDelta {
				s.pending += d.Text
			}
			prefix, err := utf8Prefix(s.pending)
			if err != nil {
				c.err = err
				break
			}
			if prefix > 0 {
				text := s.pending[:prefix]
				s.pending = s.pending[prefix:]
				s.body.WriteString(text)
			}
			if (s.category == "commentary" || s.category == "final") && s.body.Len() > s.published {
				if !s.started {
					changes = append(changes, PublicDelta{MessageID: s.id, Kind: "start", Category: s.category})
					s.started = true
				}
				changes = append(changes, PublicDelta{MessageID: s.id, Kind: "text", Category: s.category, Text: s.body.String()[s.published:]})
				s.published = s.body.Len()
			}
			if d.Kind == MessageEnd {
				if s.category == "" && strings.TrimSpace(s.body.String()) != "" {
					c.err = outputError("assistant text has no explicit native category")
					break
				}
				if s.pending != "" {
					c.err = outputError("incomplete UTF-8 in assistant output")
					break
				}
				if s.category == "commentary" || s.category == "final" {
					if !s.started {
						changes = append(changes, PublicDelta{MessageID: s.id, Kind: "start", Category: s.category})
					}
					changes = append(changes, PublicDelta{MessageID: s.id, Kind: "end", Category: s.category})
					s.output = append(s.output, PublicMessage{MessageID: s.id, Category: s.category, Text: s.body.String()})
				}
			}
		} else {
			changes, c.err = s.parser.feed(d.Text, d.Kind == MessageEnd)
			for i := range changes {
				changes[i].MessageID = s.id
			}
			if d.Kind == MessageEnd && c.err == nil {
				s.output = s.parser.output
				for i := range s.output {
					s.output[i].MessageID = s.id
				}
			}
		}
	}
	for _, change := range changes {
		if change.Kind == "start" {
			c.blocks++
		}
	}
	if c.blocks > MaxResponseBlocks {
		c.err = outputError("too many visible message blocks")
	}
	return changes, c.err
}

func (c *Classifier) Output() ([]PublicMessage, error) {
	if c.err != nil {
		return nil, c.err
	}
	result := []PublicMessage{}
	for _, source := range c.sources {
		result = append(result, source.output...)
	}
	return result, nil
}

func utf8Prefix(text string) (int, error) {
	i := 0
	for i < len(text) {
		if !utf8.FullRuneInString(text[i:]) {
			break
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			return 0, outputError("invalid UTF-8 in assistant output")
		}
		i += size
	}
	return i, nil
}

type markerParser struct {
	pending  strings.Builder
	open     string
	final    bool
	fence    byte
	fenceLen int
	body     strings.Builder
	output   []PublicMessage
	bytes    int
}

func (p *markerParser) feed(text string, finish bool) ([]PublicDelta, error) {
	p.bytes += len(text)
	if p.bytes > MaxBlockBytes {
		return nil, outputError("tagged message exceeds classification budget")
	}
	var changes []PublicDelta
	var batch strings.Builder
	var batchIndex int
	var batchCategory string
	flush := func() {
		if batch.Len() == 0 {
			return
		}
		changes = append(changes, PublicDelta{Index: batchIndex, Kind: "text", Category: batchCategory, Text: batch.String()})
		batch.Reset()
	}
	consume := func(raw string, newline bool) error {
		line := raw
		if newline {
			line = strings.TrimSuffix(raw[:len(raw)-1], "\r")
		}
		if !utf8.ValidString(raw) {
			return outputError("invalid UTF-8 in tagged output")
		}
		deltas, err := p.line(line, raw)
		for _, delta := range deltas {
			if delta.Kind == "text" {
				batchIndex, batchCategory = delta.Index, delta.Category
				batch.WriteString(delta.Text)
			} else {
				flush()
				changes = append(changes, delta)
			}
		}
		if err != nil {
			flush()
		}
		return err
	}
	for text != "" {
		end := strings.IndexByte(text, '\n')
		if end < 0 {
			p.pending.WriteString(text)
			break
		}
		p.pending.WriteString(text[:end+1])
		text = text[end+1:]
		raw := p.pending.String()
		p.pending.Reset()
		if err := consume(raw, true); err != nil {
			flush()
			return changes, err
		}
	}
	if finish && p.pending.Len() > 0 {
		raw := p.pending.String()
		p.pending.Reset()
		if err := consume(raw, false); err != nil {
			flush()
			return changes, err
		}
	}
	if finish && p.open != "" {
		flush()
		return changes, outputError("assistant output is missing a closing category tag")
	}
	flush()
	return changes, nil
}

func (p *markerParser) line(line, raw string) ([]PublicDelta, error) {
	index := len(p.output)
	change := func(kind, category, text string) []PublicDelta {
		return []PublicDelta{{Index: index, Kind: kind, Category: category, Text: text}}
	}
	if p.fence == 0 {
		switch line {
		case "<commentary>", "<final>":
			if len(p.output) >= MaxResponseBlocks {
				return nil, outputError("too many visible message blocks")
			}
			if p.open != "" {
				return nil, outputError("category blocks cannot nest")
			}
			if p.final {
				return nil, outputError("visible output follows final")
			}
			p.open = strings.TrimSuffix(strings.TrimPrefix(line, "<"), ">")
			p.body.Reset()
			return change("start", p.open, ""), nil
		case "</commentary>", "</final>":
			category := strings.TrimSuffix(strings.TrimPrefix(line, "</"), ">")
			if p.open != category {
				return nil, outputError("unexpected or mismatched closing category tag")
			}
			p.output = append(p.output, PublicMessage{Index: index, Category: p.open, Text: p.body.String()})
			p.final = p.open == "final"
			p.open = ""
			return change("end", category, ""), nil
		}
	}
	if p.open == "" {
		if strings.TrimSpace(line) == "" {
			return nil, nil
		}
		return nil, outputError("visible text occurs outside category blocks")
	}
	// Fences follow CommonMark's 0–3 spaces, >=3 matching markers and closing
	// marker length. Quotes/escapes/indented tag examples remain literal text.
	trimmed := strings.TrimLeft(line, " ")
	indent := len(line) - len(trimmed)
	if indent <= 3 && trimmed != "" && (trimmed[0] == '`' || trimmed[0] == '~') {
		marker := trimmed[0]
		n := 0
		for n < len(trimmed) && trimmed[n] == marker {
			n++
		}
		if p.fence != 0 {
			if marker == p.fence && n >= p.fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				p.fence, p.fenceLen = 0, 0
			}
		} else if n >= 3 && (marker != '`' || !strings.ContainsRune(trimmed[n:], '`')) {
			p.fence, p.fenceLen = marker, n
		}
	}
	p.body.WriteString(raw)
	return change("text", p.open, raw), nil
}
