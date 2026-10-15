package plugin

import (
	"bytes"
	"slices"
	"strings"
	"sync"

	"zende.local/harness/internal/process"
)

type Diagnostics struct {
	Text         string `json:"text"`
	BytesRead    int64  `json:"bytes_read"`
	BytesDropped int64  `json:"bytes_dropped"`
}

// Redact before retaining bytes, including matches split across stderr reads.
// Diagnostics are observational, not task output or a durable result artifact.
type diagnosticBuffer struct {
	mu      sync.Mutex
	pending []byte
	secrets []string
	keep    int
	text    []byte
	read    int64
	dropped int64
}

func newDiagnostics(secrets []string) *diagnosticBuffer {
	d := &diagnosticBuffer{keep: 1}
	for _, secret := range secrets {
		if secret != "" && !slices.Contains(d.secrets, secret) {
			d.secrets = append(d.secrets, secret)
			d.keep = max(d.keep, len(secret))
		}
	}
	// Prefer the longest value when two credentials share a prefix.
	slices.SortFunc(d.secrets, func(a, b string) int { return len(b) - len(a) })
	return d
}

func (d *diagnosticBuffer) write(data []byte, final bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.read += int64(len(data))
	d.pending = append(d.pending, data...)
	for len(d.pending) > 0 {
		safe := len(d.pending) - d.keep + 1
		if final {
			safe = len(d.pending)
		}
		if safe <= 0 {
			break
		}
		start, size := -1, 0
		for _, secret := range d.secrets {
			if at := bytes.Index(d.pending, []byte(secret)); at >= 0 && (start < 0 || at < start) {
				start, size = at, len(secret)
			}
		}
		if start >= 0 && start < safe {
			d.append(d.pending[:start])
			d.append([]byte("[REDACTED]"))
			d.pending = d.pending[start+size:]
		} else {
			d.append(d.pending[:safe])
			d.pending = d.pending[safe:]
		}
	}
	// Do not retain a large input allocation just for the look-behind tail.
	d.pending = bytes.Clone(d.pending)
}

func (d *diagnosticBuffer) append(data []byte) {
	if len(data) >= MaxDiagnosticBytes {
		d.dropped += int64(len(d.text) + len(data) - MaxDiagnosticBytes)
		d.text = bytes.Clone(data[len(data)-MaxDiagnosticBytes:])
		return
	}
	if extra := len(d.text) + len(data) - MaxDiagnosticBytes; extra > 0 {
		d.dropped += int64(extra)
		d.text = append(d.text[:0], d.text[extra:]...)
	}
	d.text = append(d.text, data...)
}

func (d *diagnosticBuffer) snapshot() Diagnostics {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Diagnostics{Text: process.DisplayText(d.text), BytesRead: d.read, BytesDropped: d.dropped}
}

func sensitiveEnv(key string) bool {
	key = strings.ToUpper(key)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
