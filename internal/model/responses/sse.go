// Package responses implements the HTTP Responses protocol. It does not own
// tools, tasks, credentials storage or the UI.
package responses

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

func wireError(message string) error { return protocol.E("model_protocol", "Responses: "+message) }

// splitLine accepts the SSE line endings LF, CRLF and CR, including a CRLF
// split across network reads. Scanner imposes a per-line memory bound.
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

// Returning done from receive stops at the validated terminal event. EOF and
// [DONE] alone never authorize completion; a trailing partial event is ignored.
func readSSE(reader io.Reader, receive func(string, []byte) (bool, error)) error {
	limited := &io.LimitedReader{R: reader, N: maxWireBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	scanner.Split(splitLine)
	var data bytes.Buffer
	event, first, haveData := "", true, false
	for scanner.Scan() {
		if limited.N <= 0 {
			return protocol.E("output_limit", "Responses stream exceeds wire budget")
		}
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if !utf8.ValidString(line) {
			return wireError("SSE line is not UTF-8")
		}
		if line == "" {
			if haveData {
				payload := data.Bytes()
				payload = bytes.TrimSuffix(payload, []byte{'\n'})
				if bytes.Equal(payload, []byte("[DONE]")) {
					return protocol.E("model_incomplete", "Responses sentinel arrived without response.completed")
				}
				done, err := receive(event, payload)
				if err != nil {
					return err
				}
				if done {
					return nil
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
				return protocol.E("output_limit", "Responses SSE event exceeds budget")
			}
			data.WriteString(value)
			data.WriteByte('\n')
			haveData = true
		}
	}
	if limited.N <= 0 {
		return protocol.E("output_limit", "Responses stream exceeds wire budget")
	}
	if scanner.Err() != nil {
		return protocol.E("model_stream", "Responses stream read failed or a line exceeded the limit")
	}
	return protocol.E("model_incomplete", "Responses stream ended without response.completed")
}
