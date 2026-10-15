package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/mcp"
)

const mcpCallSchema = `
CREATE TABLE mcp_calls (
 operation_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id),
 task_id TEXT NOT NULL REFERENCES tasks(id), fingerprint TEXT NOT NULL, owner TEXT NOT NULL,
 intent TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('dispatched','completed','failed','unknown')),
 result TEXT NOT NULL DEFAULT 'null', failure TEXT NOT NULL DEFAULT 'null'
);
CREATE INDEX mcp_calls_agent ON mcp_calls(agent_id,operation_id);
CREATE TRIGGER mcp_intent_immutable BEFORE UPDATE OF operation_id,agent_id,task_id,fingerprint,owner,intent ON mcp_calls
 BEGIN SELECT RAISE(ABORT,'immutable MCP dispatch'); END;
CREATE TRIGGER mcp_result_immutable BEFORE UPDATE ON mcp_calls WHEN OLD.state<>'dispatched'
 BEGIN SELECT RAISE(ABORT,'immutable MCP result'); END;
CREATE TRIGGER mcp_call_retained BEFORE DELETE ON mcp_calls
 BEGIN SELECT RAISE(ABORT,'retained MCP call'); END;
PRAGMA user_version=14;
`

type MCPInvocation struct {
	OperationID string                  `json:"operation_id"`
	AgentID     string                  `json:"agent_id"`
	TaskID      string                  `json:"task_id"`
	Input       protocol.MCPInvoke      `json:"input"`
	Binding     protocol.MCPCallBinding `json:"binding"`
}
type MCPRecord struct {
	Invocation MCPInvocation
	Owner      string
	Receipt    protocol.MCPCallReceipt
}

func mcpFingerprint(input MCPInvocation) (string, error) {
	req, err := protocol.NewRequest("mcp.invoke", input.OperationID, struct {
		AgentID string             `json:"agent_id"`
		TaskID  string             `json:"task_id"`
		Input   protocol.MCPInvoke `json:"input"`
	}{input.AgentID, input.TaskID, input.Input})
	if err != nil {
		return "", protocol.E("invalid_params", "MCP invocation is not encodable")
	}
	return req.Fingerprint()
}

func mcpRecord(ctx context.Context, q rowQuerier, target protocol.MCPCallTarget) (*MCPRecord, string, error) {
	var intent, result, failure, hash, state, owner, task string
	err := q.QueryRowContext(ctx, `SELECT intent,result,failure,fingerprint,state,owner,task_id FROM mcp_calls WHERE agent_id=? AND operation_id=?`, target.AgentID, target.OperationID).Scan(&intent, &result, &failure, &hash, &state, &owner, &task)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var record MCPRecord
	if len(intent) > 2<<20 || len(result) > 128<<10 || len(failure) > 4096 || json.Unmarshal([]byte(intent), &record.Invocation) != nil || json.Unmarshal([]byte(result), &record.Receipt.Result) != nil || json.Unmarshal([]byte(failure), &record.Receipt.Error) != nil {
		return nil, "", protocol.E("invalid_state", "saved MCP call is unreadable")
	}
	i, b := record.Invocation, record.Invocation.Binding
	record.Owner = owner
	r := &record.Receipt
	r.OperationID, r.AgentID, r.TaskID, r.State = i.OperationID, i.AgentID, i.TaskID, state
	r.ResourceID, r.ResourceVersion, r.Generation, r.CatalogVersion, r.ToolRef = b.ResourceID, b.ResourceVersion, b.Generation, b.CatalogVersion, b.ToolRef
	want, err := mcpFingerprint(i)
	if err != nil || want != hash || i.OperationID != target.OperationID || i.AgentID != target.AgentID || i.TaskID != task || owner == "" || b.ResourceID != i.Input.ServerID || b.ToolRef != i.Input.ToolRef || b.Generation <= 0 {
		return nil, "", protocol.E("invalid_state", "saved MCP invocation identity changed")
	}
	switch state {
	case "dispatched":
		if r.Result != nil || r.Error != nil {
			return nil, "", protocol.E("invalid_state", "pending MCP call already has a result")
		}
	case "completed":
		if r.Result == nil || r.Error != nil {
			return nil, "", protocol.E("invalid_state", "completed MCP call has no verified result")
		}
	case "failed", "unknown":
		if r.Error == nil {
			return nil, "", protocol.E("invalid_state", "MCP failure has no diagnosis")
		}
	default:
		return nil, "", protocol.E("invalid_state", "unrecognized MCP call state")
	}
	return &record, hash, nil
}

func (s *Store) MCPCall(ctx context.Context, target protocol.MCPCallTarget) (*MCPRecord, error) {
	if target.AgentID == "" || target.OperationID == "" {
		return nil, protocol.E("invalid_params", "Agent and MCP operation ID are required")
	}
	record, _, err := mcpRecord(ctx, s.db, target)
	return record, err
}

// PriorMCPCall is checked before connecting or looking up a current catalog.
// A completed original call remains readable after configuration replacement.
func (s *Store) PriorMCPCall(ctx context.Context, input MCPInvocation) (*MCPRecord, error) {
	want, err := mcpFingerprint(input)
	if err != nil {
		return nil, err
	}
	record, hash, err := mcpRecord(ctx, s.db, protocol.MCPCallTarget{AgentID: input.AgentID, OperationID: input.OperationID})
	if err == nil && record != nil && want != hash {
		return nil, protocol.E("operation_conflict", "MCP invocation ID was already used with different arguments")
	}
	return record, err
}

// BeginMCPCall commits the exact published definition before tools/call. The
// tool.dispatching journal proves provenance; there is no user-created call ID.
// A duplicate returns its original record and never grants dispatch again.
func (s *Store) BeginMCPCall(ctx context.Context, input MCPInvocation, owner string) (*MCPRecord, bool, error) {
	want, err := mcpFingerprint(input)
	if err != nil {
		return nil, false, err
	}
	if input.OperationID == "" || len(input.OperationID) > 128 || owner == "" || len(owner) > 128 || len(input.Input.Arguments) > mcp.MaxArgumentBytes {
		return nil, false, protocol.E("invalid_params", "bounded MCP invocation identity and arguments are required")
	}
	var record *MCPRecord
	fresh := false
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var hash string
		var err error
		record, hash, err = mcpRecord(ctx, tx, protocol.MCPCallTarget{AgentID: input.AgentID, OperationID: input.OperationID})
		if err != nil {
			return err
		}
		if record != nil {
			if hash != want {
				return protocol.E("operation_conflict", "MCP invocation ID was already used with different arguments")
			}
			return nil
		}
		var occupied int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM mcp_calls WHERE operation_id=?", input.OperationID).Scan(&occupied); err != nil {
			return err
		}
		if occupied != 0 {
			return protocol.E("operation_conflict", "MCP invocation ID was already used")
		}
		if err := resourceAgent(ctx, tx, input.AgentID, true); err != nil {
			return err
		}
		if err := activeTask(ctx, tx, input.AgentID, input.TaskID); err != nil {
			return err
		}
		var payload []byte
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND task_id=? AND kind='tool.dispatching' AND json_extract(payload,'$.operation_id')=? ORDER BY seq LIMIT 1`, input.AgentID, input.TaskID, input.OperationID).Scan(&payload); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.E("caller_unknown", "MCP invocation has no recorded tool dispatch")
			}
			return err
		}
		var dispatch struct {
			Call model.ToolCall `json:"call"`
		}
		if json.Unmarshal(payload, &dispatch) != nil || dispatch.Call.Name != "tool_invoke" {
			return protocol.E("caller_unknown", "MCP invocation does not identify tool_invoke")
		}
		var args protocol.MCPInvoke
		if protocol.StrictDecode(dispatch.Call.Arguments, &args) != nil {
			return protocol.E("caller_unknown", "recorded MCP arguments are invalid")
		}
		original := input
		original.Input = args
		check, _ := mcpFingerprint(original)
		if check != want {
			return protocol.E("caller_unknown", "MCP arguments differ from their recorded dispatch")
		}
		b := input.Binding
		if input.Input.ServerID != b.ResourceID || input.Input.ToolRef != b.ToolRef || b.Generation <= 0 {
			return protocol.E("invalid_state", "MCP binding differs from requested target")
		}
		var packageRaw string
		if err := tx.QueryRowContext(ctx, `SELECT v.package FROM resource_versions v JOIN resource_publications p ON p.id=v.id AND p.version=v.version WHERE p.agent_id=? AND p.id=? AND p.version=? AND p.generation=? LIMIT 1`, input.AgentID, b.ResourceID, b.ResourceVersion, b.Generation).Scan(&packageRaw); err != nil {
			return protocol.E("stale_reference", "MCP binding was not published to this Agent; search again")
		}
		pkg, err := decodePackage(packageRaw)
		if err != nil {
			return err
		}
		if pkg.MCP == nil || pkg.MCP.Version != b.CatalogVersion {
			return protocol.E("invalid_state", "MCP binding catalog differs from publication")
		}
		found := false
		for _, d := range pkg.MCP.Tools {
			if d.Ref == b.ToolRef && d.ServerID == b.ResourceID && string(d.Tool) == string(b.Definition) {
				found = true
				break
			}
		}
		if !found {
			return protocol.E("stale_reference", "MCP definition was not published; search again")
		}
		raw, _ := json.Marshal(input)
		if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_calls(operation_id,agent_id,task_id,fingerprint,owner,intent,state) VALUES(?,?,?,?,?,?,'dispatched')`, input.OperationID, input.AgentID, input.TaskID, want, owner, string(raw)); err != nil {
			return err
		}
		record, _, err = mcpRecord(ctx, tx, protocol.MCPCallTarget{AgentID: input.AgentID, OperationID: input.OperationID})
		if err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, input.AgentID, input.TaskID, "mcp.dispatched", record.Receipt); err != nil {
			return err
		}
		fresh = true
		return nil
	})
	return record, fresh, err
}

// FinishMCPCall first fsyncs immutable result bytes, then commits the receipt
// and a small event. A crash between them can leave an orphan file, never a
// receipt pointing to unwritten data. Unknown responses remain evidence only.
func (s *Store) FinishMCPCall(ctx context.Context, input MCPInvocation, owner string, raw json.RawMessage, failure *protocol.Error) (*MCPRecord, error) {
	var info *protocol.MCPResultInfo
	var err error
	if len(raw) > 0 {
		info, err = s.saveMCPResult(raw)
		if err != nil {
			return nil, err
		}
	}
	if failure != nil && (len(failure.Error()) > 2048 || strings.ContainsRune(failure.Error(), 0)) {
		return nil, protocol.E("invalid_params", "MCP failure must be a bounded host diagnostic")
	}
	state := "completed"
	if failure != nil {
		state = "failed"
		if failure.Data.Kind == "operation_unknown" || failure.Data.Kind == "transport_unknown" {
			state = "unknown"
		}
	}
	if state == "completed" && info == nil {
		return nil, protocol.E("invalid_state", "completed MCP call needs retained result bytes")
	}
	var record *MCPRecord
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var hash string
		var err error
		record, hash, err = mcpRecord(ctx, tx, protocol.MCPCallTarget{AgentID: input.AgentID, OperationID: input.OperationID})
		if err != nil {
			return err
		}
		want, _ := mcpFingerprint(input)
		if record == nil || hash != want || record.Owner != owner || !reflect.DeepEqual(record.Invocation.Binding, input.Binding) {
			return protocol.E("operation_conflict", "MCP completion does not match its original dispatch")
		}
		if record.Receipt.State != "dispatched" {
			if record.Receipt.State == state && reflect.DeepEqual(record.Receipt.Result, info) && reflect.DeepEqual(record.Receipt.Error, failure) {
				return nil
			}
			return protocol.E("operation_conflict", "MCP completion is already recorded")
		}
		rawInfo, _ := json.Marshal(info)
		rawError, _ := json.Marshal(failure)
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_calls SET state=?,result=?,failure=? WHERE operation_id=?`, state, string(rawInfo), string(rawError), input.OperationID); err != nil {
			return err
		}
		record.Receipt.State, record.Receipt.Result, record.Receipt.Error = state, info, failure
		_, err = appendEvent(ctx, tx, input.AgentID, input.TaskID, "mcp."+state, record.Receipt)
		return err
	})
	return record, err
}

// The elected replacement core has no evidence that an old dispatched call
// completed. Preserve its identity, and never replay it during recovery.
func (s *Store) InterruptMCPCalls(ctx context.Context, owner string) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT agent_id,operation_id FROM mcp_calls WHERE state='dispatched' AND owner<>?`, owner)
		if err != nil {
			return err
		}
		var targets []protocol.MCPCallTarget
		for rows.Next() {
			var p protocol.MCPCallTarget
			if err = rows.Scan(&p.AgentID, &p.OperationID); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		failure := protocol.E("operation_unknown", "core restarted without a retained MCP result; inspect the original operation, do not repeat it")
		raw, _ := json.Marshal(failure)
		for _, p := range targets {
			record, _, err := mcpRecord(ctx, tx, p)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE mcp_calls SET state='unknown',failure=? WHERE operation_id=?`, string(raw), p.OperationID); err != nil {
				return err
			}
			record.Receipt.State, record.Receipt.Error = "unknown", failure
			if _, err = appendEvent(ctx, tx, p.AgentID, record.Invocation.TaskID, "mcp.unknown", record.Receipt); err != nil {
				return err
			}
		}
		return nil
	})
}
