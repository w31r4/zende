package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const MaxConcurrentRequests = 8
const MaxQueuedProgress = 32
const MaxProgressBytes = 16 << 10
const WriteTimeout = 2 * time.Second
const CancelTimeout = 5 * time.Second

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Progress struct {
	RequestID   string          `json:"request_id"`
	OperationID string          `json:"operation_id"`
	Sequence    int64           `json:"sequence"`
	Value       json.RawMessage `json:"value"`
}

type pendingResponse struct {
	id        string
	operation string
	sequence  int64 // owned by the protocol reader while mu is held
	complete  bool
	ch        chan response // progress and final share wire order
}

func (i *Instance) forget(p *pendingResponse) {
	i.mu.Lock()
	delete(i.pending, p.id)
	i.mu.Unlock()
}

func (i *Instance) startRequest(ctx context.Context, method string, params any, operation string) (*pendingResponse, bool, error) {
	p := &pendingResponse{id: protocol.NewID("rpc"), operation: operation, ch: make(chan response, MaxQueuedProgress+1)}
	raw, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", p.id, method, params})
	if err != nil || len(raw) > MaxFrameBytes {
		return p, false, protocol.E("invalid_params", "plugin request exceeds the protocol budget")
	}
	i.mu.Lock()
	if i.info.State == "closed" || i.info.State == "failed" || operation != "" && i.info.State != "ready" {
		i.mu.Unlock()
		return p, false, protocol.E("unavailable", "plugin connection is not ready")
	}
	if len(i.pending) >= MaxConcurrentRequests {
		i.mu.Unlock()
		return p, false, protocol.E("resource_limit", "plugin permits at most 8 concurrent requests")
	}
	i.pending[p.id] = p
	i.mu.Unlock()
	n, err := i.writeFrame(ctx, raw)
	if err != nil {
		i.forget(p)
	}
	return p, n > 0, err
}

// Serialize complete frames, not whole requests. Independent invocations may
// interleave responses; cancellation must remain writable during another call.
func (i *Instance) writeFrame(ctx context.Context, raw []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
	select {
	case i.gate <- struct{}{}:
		defer func() { <-i.gate }()
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-i.done:
		return 0, protocol.E("unavailable", "plugin connection is closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	select {
	case <-i.done:
		return 0, protocol.E("unavailable", "plugin connection is closed")
	default:
	}
	deadline, _ := ctx.Deadline()
	_ = i.stdin.SetWriteDeadline(deadline)
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = i.stdin.SetWriteDeadline(time.Now()); close(interrupted) })
	n, err := i.stdin.Write(append(bytes.Clone(raw), '\n'))
	if !stop() {
		<-interrupted
	}
	_ = i.stdin.SetWriteDeadline(time.Time{})
	if err != nil {
		// A partial frame poisons the stream. Never append another request to it.
		i.stop(protocol.E("plugin_protocol", "plugin request could not be written"))
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return n, context.DeadlineExceeded
		}
		return n, protocol.E("plugin_protocol", "plugin request could not be written")
	}
	return n, nil
}

func (i *Instance) cancelRequest(p *pendingResponse) error {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "plugin.cancel", "params": map[string]string{"request_id": p.id, "operation_id": p.operation}})
	_, err := i.writeFrame(context.Background(), raw)
	return err
}

func (i *Instance) readProtocol() {
	defer i.readers.Done()
	scanner := bufio.NewScanner(i.stdout)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes+2)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if at := bytes.IndexByte(data, '\n'); at >= 0 {
			return at + 1, bytes.TrimSuffix(data[:at], []byte{'\r'}), nil
		}
		if atEOF && len(data) != 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		line := scanner.Bytes()
		var frame response
		var fields map[string]json.RawMessage
		if len(line) > MaxFrameBytes || !utf8.Valid(line) || protocol.StrictDecode(line, &frame) != nil || json.Unmarshal(line, &fields) != nil || len(fields) != 3 || frame.JSONRPC != "2.0" || !i.route(frame) {
			i.stop(protocol.E("plugin_protocol", "invalid, unsolicited or overflowing plugin stdout frame; logs must use stderr"))
			return
		}
	}
	i.stop(protocol.E("plugin_protocol", "plugin stdout ended or exceeded its frame budget"))
}

func (i *Instance) route(frame response) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if frame.Method != "" {
		if frame.Method != "plugin.progress" || frame.ID != "" || len(frame.Result) != 0 || len(frame.Error) != 0 || len(frame.Params) > MaxProgressBytes {
			return false
		}
		var progress Progress
		if protocol.StrictDecode(frame.Params, &progress) != nil || len(progress.Value) == 0 {
			return false
		}
		if _, err := decodeValue(progress.Value, MaxProgressBytes); err != nil {
			return false
		}
		p := i.pending[progress.RequestID]
		if p == nil || p.complete || p.operation == "" || p.operation != progress.OperationID || progress.Sequence != p.sequence+1 || progress.Sequence < 1 || len(p.ch) >= MaxQueuedProgress {
			return false
		}
		p.sequence = progress.Sequence
		p.ch <- frame
		return true
	}
	if frame.ID == "" || len(frame.ID) > 128 || len(frame.Params) != 0 || (len(frame.Result) == 0) == (len(frame.Error) == 0) {
		return false
	}
	if len(frame.Error) > 0 {
		var remote struct {
			Code    *int            `json:"code"`
			Message *string         `json:"message"`
			Data    json.RawMessage `json:"data,omitempty"`
		}
		if protocol.StrictDecode(frame.Error, &remote) != nil || remote.Code == nil || remote.Message == nil {
			return false
		}
	}
	p := i.pending[frame.ID]
	if p == nil || p.complete {
		return false
	}
	p.complete = true
	p.ch <- frame // one reserved slot for the final response
	return true
}
