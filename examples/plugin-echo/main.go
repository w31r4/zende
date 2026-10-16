// plugin-echo is a standalone Zende protocol-v1 example using only Go's
// standard library. It echoes text; delay_ms makes progress/cancellation visible.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
)

const maxFrame = 8 << 20

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type call struct {
	OperationID   string `json:"operation_id"`
	DefinitionRef string `json:"definition_ref"`
	Name          string `json:"name"`
	Scope         struct {
		AgentID string `json:"agent_id"`
		TaskID  string `json:"task_id"`
		Cwd     string `json:"cwd"`
	} `json:"scope"`
	Arguments struct {
		Text    string `json:"text"`
		DelayMS int    `json:"delay_ms"`
	} `json:"arguments"`
}

type pending struct {
	call
	due time.Time
}

type inputFrame struct {
	request
	err error
}

func main() {
	if err := serve(os.Stdin, os.Stdout); err != nil {
		// Never print diagnostics to the protocol stream.
		fmt.Fprintln(os.Stderr, "plugin-echo:", err)
		os.Exit(1)
	}
}

func serve(input io.ReadCloser, output io.Writer) error {
	frames, done, readerDone := make(chan inputFrame), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), maxFrame+2)
		for scanner.Scan() {
			var f inputFrame
			f.err = json.Unmarshal(scanner.Bytes(), &f.request)
			if len(scanner.Bytes()) > maxFrame {
				f.err = errors.New("request exceeds frame budget")
			}
			select {
			case frames <- f:
			case <-done:
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case frames <- inputFrame{err: err}:
		case <-done:
		}
	}()
	defer func() { close(done); input.Close(); <-readerDone }()

	// One writer/event loop serializes whole frames. Waiting calls do not stop
	// the reader from receiving another call, cancellation or shutdown.
	encoder := json.NewEncoder(output)
	respond := func(id string, result any) error {
		return encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	failure := func(id string, code int, message string) error {
		return encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	}
	complete := func(id string, p pending, state string) error {
		r := map[string]any{"operation_id": p.OperationID, "state": state}
		if state == "completed" {
			r["output"] = map[string]string{"text": p.Arguments.Text}
		}
		return respond(id, r)
	}
	active := map[string]pending{}
	ready := false
	ticks := time.NewTicker(10 * time.Millisecond)
	defer ticks.Stop()
	for {
		select {
		case now := <-ticks.C:
			for id, p := range active {
				if !now.Before(p.due) {
					if err := complete(id, p, "completed"); err != nil {
						return err
					}
					delete(active, id)
				}
			}
		case f := <-frames:
			if errors.Is(f.err, io.EOF) {
				return nil
			}
			if f.err != nil || f.JSONRPC != "2.0" {
				return errors.New("invalid request frame")
			}
			if f.Method == "plugin.cancel" {
				var cancel struct {
					RequestID   string `json:"request_id"`
					OperationID string `json:"operation_id"`
				}
				if f.ID != "" || json.Unmarshal(f.Params, &cancel) != nil {
					return errors.New("invalid cancellation")
				}
				if p, ok := active[cancel.RequestID]; ok && p.OperationID == cancel.OperationID {
					if err := complete(cancel.RequestID, p, "cancelled"); err != nil {
						return err
					}
					delete(active, cancel.RequestID)
				}
				// Cancellation arriving after completion has no new response.
				continue
			}
			if f.ID == "" {
				return errors.New("request ID required")
			}
			if _, duplicate := active[f.ID]; duplicate {
				return errors.New("duplicate in-flight request ID")
			}
			if !ready && f.Method != "plugin.initialize" {
				return errors.New("initialize before using capabilities")
			}
			var err error
			switch f.Method {
			case "plugin.initialize":
				var p struct {
					ProtocolVersions []int `json:"protocol_versions"`
					Plugin           struct {
						ID      string `json:"id"`
						Version string `json:"version"`
					} `json:"plugin"`
					Capabilities []string `json:"capabilities"`
				}
				if json.Unmarshal(f.Params, &p) != nil {
					return errors.New("invalid initialization")
				}
				slices.Sort(p.Capabilities)
				if ready || !slices.Contains(p.ProtocolVersions, 1) || p.Plugin.ID != "example-echo" || p.Plugin.Version != "1.0.0" || !slices.Equal(p.Capabilities, []string{"commands", "tools"}) {
					err = failure(f.ID, -32602, "expected example-echo 1.0.0 with tools/commands and protocol 1")
				} else {
					err = respond(f.ID, map[string]any{"protocol_version": 1, "plugin": p.Plugin, "capabilities": p.Capabilities})
					ready = true
				}
			case "plugin.describe":
				definition := map[string]any{
					"name": "echo", "description": "Echo text after an optional delay; cancellation returns a confirmed terminal result.",
					"input_schema":  json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string","maxLength":4096},"delay_ms":{"type":"integer","minimum":0,"maximum":60000}},"additionalProperties":false}`),
					"output_schema": json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`),
				}
				err = respond(f.ID, map[string]any{"tools": []any{definition}, "commands": []any{definition}})
			case "plugin.tool.call", "plugin.command.call":
				var p pending
				if json.Unmarshal(f.Params, &p.call) != nil || p.OperationID == "" || p.DefinitionRef == "" || p.Name != "echo" || p.Arguments.DelayMS < 0 || p.Arguments.DelayMS > 60000 {
					return errors.New("invalid invocation")
				}
				if len(active) >= 8 {
					err = respond(f.ID, map[string]any{"operation_id": p.OperationID, "state": "failed", "error": map[string]string{"kind": "busy", "message": "at most 8 active calls"}})
					break
				}
				err = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "plugin.progress", "params": map[string]any{"request_id": f.ID, "operation_id": p.OperationID, "sequence": 1, "value": map[string]string{"stage": "waiting"}}})
				p.due = time.Now().Add(time.Duration(p.Arguments.DelayMS) * time.Millisecond)
				active[f.ID] = p
			case "plugin.shutdown":
				return respond(f.ID, nil)
			default:
				err = failure(f.ID, -32601, "method not supported")
			}
			if err != nil {
				return err
			}
		}
	}
}
