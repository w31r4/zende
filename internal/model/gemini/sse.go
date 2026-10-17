package gemini

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const maxEventBytes = 8 << 20
const maxWireBytes = 64 << 20

func splitLine(data []byte, eof bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
		if b == '\r' {
			if i+1 == len(data) && !eof {
				return 0, nil, nil
			}
			n := i + 1
			if n < len(data) && data[n] == '\n' {
				n++
			}
			return n, data[:i], nil
		}
	}
	if eof && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func readSSE(reader io.Reader, receive func(string, []byte) error) error {
	limited := &io.LimitedReader{R: reader, N: maxWireBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	scanner.Split(splitLine)
	var data bytes.Buffer
	event, first, haveData := "", true, false
	for scanner.Scan() {
		if limited.N <= 0 {
			return protocol.E("output_limit", "Gemini stream exceeds wire budget")
		}
		line := scanner.Text()
		if first {
			line, first = strings.TrimPrefix(line, "\ufeff"), false
		}
		if !utf8.ValidString(line) {
			return wireError("SSE line is not UTF-8")
		}
		if line == "" {
			if haveData {
				if err := receive(event, bytes.TrimSuffix(data.Bytes(), []byte{'\n'})); err != nil {
					return err
				}
			}
			data.Reset()
			event, haveData = "", false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			event = value
		case "data":
			if data.Len()+len(value)+1 > maxEventBytes {
				return protocol.E("output_limit", "Gemini SSE event exceeds budget")
			}
			data.WriteString(value)
			data.WriteByte('\n')
			haveData = true
		}
	}
	if limited.N <= 0 {
		return protocol.E("output_limit", "Gemini stream exceeds wire budget")
	}
	if scanner.Err() != nil {
		return protocol.E("model_stream", "Gemini stream read failed or a line exceeded the limit")
	}
	if haveData {
		return protocol.E("model_incomplete", "Gemini stream ended within an SSE event")
	}
	return nil
}
