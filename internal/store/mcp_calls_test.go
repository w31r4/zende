package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

// This fixture represents the durable boundary reached by BeginMCPCall. The
// native runtime suite verifies publication/schema/dispatch provenance.
func savedMCPDispatch(t *testing.T, s *Store) MCPInvocation {
	t.Helper()
	_, a := create(t, s)
	input := MCPInvocation{AgentID: a.AgentID, TaskID: a.TaskID, OperationID: "original", Input: protocol.MCPInvoke{ServerID: "server", ToolRef: "ref", Arguments: json.RawMessage(`{}`)}, Binding: protocol.MCPCallBinding{ResourceID: "server", ResourceVersion: strings.Repeat("a", 64), Generation: 1, CatalogVersion: strings.Repeat("b", 64), ToolRef: "ref", Definition: json.RawMessage(`{"name":"fixture"}`)}}
	hash, _ := mcpFingerprint(input)
	raw, _ := json.Marshal(input)
	if _, err := s.db.Exec(`INSERT INTO mcp_calls(operation_id,agent_id,task_id,fingerprint,owner,intent,state) VALUES(?,?,?,?,?,?,'dispatched')`, input.OperationID, input.AgentID, input.TaskID, hash, "owner", string(raw)); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestMCPReceiptAtomicCompletionAndRetention(t *testing.T) {
	s, path := newStore(t)
	input := savedMCPDispatch(t, s)
	ctx := t.Context()
	raw := json.RawMessage(`{"content":[{"type":"text","text":"hello"}],"structuredContent":{"ok":true},"isError":false}`)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_mcp_event BEFORE INSERT ON events WHEN NEW.kind='mcp.completed' BEGIN SELECT RAISE(ABORT,'injected'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishMCPCall(ctx, input, "owner", raw, nil); err == nil {
		t.Fatal("expected commit failure")
	}
	r, err := s.MCPCall(ctx, protocol.MCPCallTarget{AgentID: input.AgentID, OperationID: input.OperationID})
	if err != nil || r.Receipt.State != "dispatched" || r.Receipt.Result != nil {
		t.Fatal(r, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_mcp_event"); err != nil {
		t.Fatal(err)
	}
	r, err = s.FinishMCPCall(ctx, input, "owner", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		again, err := s.FinishMCPCall(ctx, input, "owner", raw, nil)
		if err != nil || !reflect.DeepEqual(r, again) {
			t.Fatal(again, err)
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM events WHERE kind='mcp.completed'").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	for _, statement := range []string{"DELETE FROM mcp_calls", "UPDATE mcp_calls SET owner='other'", "UPDATE mcp_calls SET result='null'", "UPDATE mcp_calls SET state='dispatched'"} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatal("mutated retained record", statement)
		}
	}
	if _, err := s.FinishMCPCall(ctx, input, "owner", json.RawMessage(`{"content":[]}`), nil); err == nil {
		t.Fatal("replaced result")
	}
	if _, err := s.FinishMCPCall(ctx, input, "other", raw, nil); err == nil {
		t.Fatal("wrong completion owner")
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.InterruptMCPCalls(ctx, "replacement"); err != nil {
		t.Fatal(err)
	}
	again, err := reopened.MCPCall(ctx, protocol.MCPCallTarget{AgentID: input.AgentID, OperationID: input.OperationID})
	if err != nil || !reflect.DeepEqual(r, again) {
		t.Fatal(again, err)
	}
	page, err := reopened.ReadMCPResult(ctx, protocol.MCPResultRead{AgentID: input.AgentID, OperationID: input.OperationID, Part: "structured"})
	if err != nil || page.Text != `{"ok":true}` {
		t.Fatal(page, err)
	}
}

func TestMCPArtifactsBoundsTypesAndIntegrity(t *testing.T) {
	s, _ := newStore(t)
	input := savedMCPDispatch(t, s)
	content := []any{
		map[string]any{"type": "text", "text": strings.Repeat("你\x00", 10000)},
		map[string]any{"type": "image", "mimeType": strings.Repeat("X", 100000), "data": "AAECAw=="},
		map[string]any{"type": "audio", "mimeType": "audio/wav", "data": "AQIDBA=="},
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///never-read", "text": "inline text", "mimeType": "text/plain"}},
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "https://example.invalid", "blob": "AQIDBA==", "mimeType": "application/octet-stream"}},
	}
	for range 12 {
		content = append(content, map[string]string{"type": "text", "text": "tail"})
	}
	raw, _ := json.Marshal(map[string]any{"content": content, "isError": true, "structuredContent": map[string]string{"result": "error"}})
	r, err := s.FinishMCPCall(t.Context(), input, "owner", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := json.Marshal(r.Receipt)
	if len(summary) > 64<<10 || len(r.Receipt.Result.Parts) != 8 || !r.Receipt.Result.More || !r.Receipt.Result.IsError {
		t.Fatal("unbounded or incomplete receipt", len(summary), r.Receipt.Result)
	}
	for part, want := range map[string]string{"1": "base64", "2": "base64", "3": "utf-8", "4": "base64", "16": "utf-8"} {
		page, err := s.ReadMCPResult(t.Context(), protocol.MCPResultRead{AgentID: input.AgentID, OperationID: input.OperationID, Part: part})
		if err != nil || page.Encoding != want {
			t.Fatal(page, err)
		}
	}
	for _, p := range []protocol.MCPResultRead{{Part: "0", Offset: 1}, {Part: "0", Offset: 1 << 30}, {Part: "17"}, {Part: "-1"}, {Part: "01"}, {Part: "../x"}, {Limit: 1}, {Limit: 1 << 20}, {Offset: -1}} {
		p.AgentID, p.OperationID = input.AgentID, input.OperationID
		if _, err := s.ReadMCPResult(t.Context(), p); err == nil {
			t.Fatal("invalid page accepted", p)
		}
	}
	artifact := filepath.Join(s.mcpResultDir(), r.Receipt.Result.SHA256+".json")
	stat, err := os.Stat(artifact)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal(stat, err)
	}
	corrupt := append([]byte(nil), raw...)
	corrupt[0] = ' '
	if err := os.WriteFile(artifact, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadMCPResult(t.Context(), protocol.MCPResultRead{AgentID: input.AgentID, OperationID: input.OperationID}); err == nil {
		t.Fatal("corrupt artifact accepted")
	}
	if _, err := s.saveMCPResult(raw); err == nil {
		t.Fatal("corrupt artifact was overwritten")
	}
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readMCPResult(r.Receipt.Result); err == nil {
		t.Fatal("missing artifact accepted")
	}
	if err := unix.Mkfifo(artifact, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readMCPResult(r.Receipt.Result); err == nil {
		t.Fatal("FIFO artifact accepted")
	}
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(s.mcpResultDir(), "elsewhere.json")
	if err := os.WriteFile(elsewhere, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readMCPResult(r.Receipt.Result); err == nil {
		t.Fatal("symlink artifact accepted")
	}
}

func TestMCPSchemaMigratesVersion13(t *testing.T) {
	s, path := newStore(t)
	_, a := create(t, s)
	before, err := s.Snapshot(context.Background(), a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE mcp_calls; PRAGMA user_version=13"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.Snapshot(context.Background(), a.AgentID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal(after, err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
}
