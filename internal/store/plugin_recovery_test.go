package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func TestPluginJournalAtomicProgressAndCompletion(t *testing.T) {
	f := newPluginJournal(t, "")
	call, inv, target := f.prepare(t, "call", "tool")
	beginPlugin(t, f, inv)
	result, err := call.Invoke(t.Context(), func(p plugin.Progress) error {
		if _, err := f.s.db.Exec(`CREATE TRIGGER fail_plugin_progress BEFORE INSERT ON events WHEN NEW.kind='plugin.progress' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
			return err
		}
		if err := f.s.AppendPluginProgress(t.Context(), target, "owner", p); err == nil {
			return fmt.Errorf("expected progress commit failure")
		}
		r, err := f.s.PluginCall(t.Context(), target)
		if err != nil {
			return err
		}
		if r.Receipt.ProgressSeq != 0 || r.Receipt.RequestID != "" {
			return fmt.Errorf("uncommitted progress head visible")
		}
		var count int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM plugin_progress`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("uncommitted progress visible")
		}
		if _, err := f.s.db.Exec(`DROP TRIGGER fail_plugin_progress`); err != nil {
			return err
		}
		return f.s.AppendPluginProgress(t.Context(), target, "owner", p)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_plugin_result BEFORE INSERT ON events WHEN NEW.kind='plugin.completed' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if r, err := f.s.FinishPluginCall(t.Context(), target, "owner", result); err == nil || r != nil {
		t.Fatal("returned uncommitted receipt", r, err)
	}
	r, err := f.s.PluginCall(t.Context(), target)
	if err != nil || r.Receipt.State != "dispatched" || r.Receipt.ResultBytes != 0 {
		t.Fatal(r, err)
	}
	if _, err := f.s.db.Exec(`DROP TRIGGER fail_plugin_result`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FinishPluginCall(t.Context(), target, "owner", result); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE plugin_calls SET intent='{}'`, `UPDATE plugin_calls SET owner='other'`,
		`UPDATE plugin_calls SET result='{}'`, `UPDATE plugin_calls SET state='dispatched'`,
		`DELETE FROM plugin_calls`, `UPDATE plugin_progress SET payload='{}'`, `DELETE FROM plugin_progress`,
	} {
		if _, err := f.s.db.Exec(q); err == nil {
			t.Fatalf("history guard allowed %s", q)
		}
	}
}

func TestPluginJournalProgressValidationAndCursors(t *testing.T) {
	f := newPluginJournal(t, "")
	_, inv, target := f.prepare(t, "call", "command")
	beginPlugin(t, f, inv)
	p := plugin.Progress{RequestID: "request", OperationID: target.OperationID, Sequence: 1, Value: json.RawMessage(`{"text":"progress"}`)}
	for name, change := range map[string]func(*plugin.Progress){
		"duplicate-keys": func(p *plugin.Progress) { p.Value = json.RawMessage(`{"a":1,"a":2}`) },
		"large-frame": func(p *plugin.Progress) {
			raw, _ := json.Marshal(strings.Repeat("x", plugin.MaxProgressBytes))
			p.Value = raw
		},
		"operation": func(p *plugin.Progress) { p.OperationID = "other" },
		"sequence":  func(p *plugin.Progress) { p.Sequence = 0 },
		"request":   func(p *plugin.Progress) { p.RequestID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := p
			change(&bad)
			wantPluginError(t, f.s.AppendPluginProgress(t.Context(), target, "owner", bad), "invalid_params")
		})
	}
	wantPluginError(t, f.s.AppendPluginProgress(t.Context(), target, "other", p), "operation_conflict")
	if err := f.s.AppendPluginProgress(t.Context(), target, "owner", p); err != nil {
		t.Fatal(err)
	}
	if err := f.s.AppendPluginProgress(t.Context(), target, "owner", p); err != nil {
		t.Fatal("identical replay rejected", err)
	}
	bad := p
	bad.Value = json.RawMessage(`"different"`)
	wantPluginError(t, f.s.AppendPluginProgress(t.Context(), target, "owner", bad), "operation_conflict")
	bad = p
	bad.Sequence = 3
	wantPluginError(t, f.s.AppendPluginProgress(t.Context(), target, "owner", bad), "operation_conflict")
	bad = p
	bad.RequestID = "other"
	bad.Sequence = 2
	wantPluginError(t, f.s.AppendPluginProgress(t.Context(), target, "owner", bad), "operation_conflict")
	if _, err := f.s.MarkPluginCancelling(t.Context(), target, "owner"); err != nil {
		t.Fatal(err)
	}
	bad = p
	bad.Sequence = 2
	if err := f.s.AppendPluginProgress(t.Context(), target, "owner", bad); err != nil {
		t.Fatal("cancelling lost final progress", err)
	}
	page, err := f.s.PluginProgress(t.Context(), target, 0, 1)
	if err != nil || !page.More || page.Next != 1 {
		t.Fatal(page, err)
	}
	page, err = f.s.PluginProgress(t.Context(), target, 2, 1)
	if err != nil || page.More || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	_, err = f.s.PluginProgress(t.Context(), target, 3, 1)
	wantPluginError(t, err, "cursor_gap")
	_, err = f.s.PluginProgress(t.Context(), PluginCallTarget{AgentID: "other", OperationID: target.OperationID}, 0, 1)
	wantPluginError(t, err, "not_found")
	_, err = f.s.ReadPluginResult(t.Context(), target, 0, 0)
	wantPluginError(t, err, "unavailable")
	if err := f.s.InterruptPluginCalls(t.Context(), "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.AppendPluginProgress(t.Context(), target, "owner", p); err != nil {
		t.Fatal("terminal replay rejected", err)
	}
	bad.Sequence = 3
	wantPluginError(t, f.s.AppendPluginProgress(t.Context(), target, "owner", bad), "operation_conflict")
}

func TestPluginJournalProgressBudgets(t *testing.T) {
	f := newPluginJournal(t, "")
	for _, mode := range []string{"count", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			_, inv, target := f.prepare(t, mode, "tool")
			beginPlugin(t, f, inv)
			value := json.RawMessage(`null`)
			if mode == "bytes" {
				value, _ = json.Marshal(strings.Repeat("x", 16_000))
			}
			p := plugin.Progress{RequestID: "request", OperationID: target.OperationID, Value: value}
			var count, total int64
			for {
				p.Sequence = count + 1
				raw, _ := json.Marshal(p)
				err := f.s.AppendPluginProgress(t.Context(), target, "owner", p)
				if err != nil {
					if mode == "count" {
						wantPluginError(t, err, "invalid_params")
						if count != MaxPluginProgressEntries {
							t.Fatal(count)
						}
					} else {
						wantPluginError(t, err, "resource_limit")
						if total+int64(len(raw)) <= MaxPluginProgressBytes {
							t.Fatal(total)
						}
					}
					break
				}
				count++
				total += int64(len(raw))
			}
			r, err := f.s.PluginCall(t.Context(), target)
			if err != nil || r.Receipt.ProgressSeq != count || r.Receipt.ProgressBytes != total {
				t.Fatal(r, err)
			}
			page, err := f.s.PluginProgress(t.Context(), target, count-1, 1)
			if err != nil || page.More || len(page.Items) != 1 || page.Next != count {
				t.Fatal(page, err)
			}
		})
	}
}

func TestPluginJournalRecoveryAndNotDispatched(t *testing.T) {
	f := newPluginJournal(t, "")
	call, inv, target := f.prepare(t, "unsent", "tool")
	beginPlugin(t, f, inv)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := call.Invoke(ctx, func(plugin.Progress) error { return nil })
	if err == nil || result.State != "not_dispatched" {
		t.Fatal(result, err)
	}
	if _, err := f.s.FinishPluginCall(t.Context(), target, "owner", result); err != nil {
		t.Fatal(err)
	}
	_, pending, pendingTarget := f.prepare(t, "pending", "command")
	beginPlugin(t, f, pending)
	if _, err := f.s.MarkPluginCancelling(t.Context(), pendingTarget, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.InterruptPluginCalls(t.Context(), "owner"); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.PluginCall(t.Context(), pendingTarget)
	if err != nil || r.Receipt.State != "cancelling" {
		t.Fatal(r, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 2 {
		if err := s.InterruptPluginCalls(t.Context(), "replacement"); err != nil {
			t.Fatal(err)
		}
	}
	r, err = s.PriorPluginCall(t.Context(), pending.Request)
	if err != nil || r.Receipt.State != "unknown" || r.Receipt.ResultBytes != 0 || r.Receipt.Error.Data.Kind != "operation_unknown" {
		t.Fatal(r, err)
	}
	_, fresh, err := s.BeginPluginCall(t.Context(), pending, "replacement")
	if err != nil || fresh {
		t.Fatal(fresh, err)
	}
	r, err = s.PluginCall(t.Context(), target)
	if err != nil || r.Receipt.State != "not_dispatched" {
		t.Fatal(r, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE kind='plugin.unknown'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestPluginJournalIntegrityReaders(t *testing.T) {
	for _, part := range []string{"intent", "progress", "result"} {
		t.Run(part, func(t *testing.T) {
			f := newPluginJournal(t, "")
			call, inv, target := f.prepare(t, "call", "tool")
			beginPlugin(t, f, inv)
			result, err := call.Invoke(t.Context(), func(p plugin.Progress) error { return f.s.AppendPluginProgress(t.Context(), target, "owner", p) })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.FinishPluginCall(t.Context(), target, "owner", result); err != nil {
				t.Fatal(err)
			}
			page, err := f.s.ReadPluginResult(t.Context(), target, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			offset := strings.Index(page.Text, "中")
			if offset < 0 {
				t.Fatal("fixture lost multibyte text")
			}
			_, err = f.s.ReadPluginResult(t.Context(), target, offset+1, 4)
			wantPluginError(t, err, "invalid_params")
			_, err = f.s.ReadPluginResult(t.Context(), target, page.Total+1, 4)
			wantPluginError(t, err, "invalid_params")
			// Corrupt only this disposable database, bypassing its history guards.
			switch part {
			case "intent":
				_, err = f.s.db.Exec(`DROP TRIGGER plugin_intent_immutable; DROP TRIGGER plugin_result_immutable; UPDATE plugin_calls SET intent='{}'`)
			case "progress":
				_, err = f.s.db.Exec(`DROP TRIGGER plugin_progress_no_update; UPDATE plugin_progress SET payload='{}'`)
			case "result":
				_, err = f.s.db.Exec(`DROP TRIGGER plugin_result_immutable; UPDATE plugin_calls SET result='{}'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			switch part {
			case "intent":
				_, err = f.s.PluginCall(t.Context(), target)
			case "progress":
				_, err = f.s.PluginProgress(t.Context(), target, 0, 0)
			case "result":
				_, err = f.s.ReadPluginResult(t.Context(), target, 0, 0)
			}
			wantPluginError(t, err, "invalid_state")
		})
	}
}

func TestPluginSchemaMigratesVersion14(t *testing.T) {
	s, path := newStore(t)
	input := savedMCPDispatch(t, s)
	ctx := t.Context()
	before, err := s.Snapshot(ctx, input.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	mcpBefore, err := s.MCPCall(ctx, protocol.MCPCallTarget{AgentID: input.AgentID, OperationID: input.OperationID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE plugin_progress; DROP TABLE plugin_calls; PRAGMA user_version=14`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.Snapshot(ctx, input.AgentID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal(after, err)
	}
	mcpAfter, err := s.MCPCall(ctx, protocol.MCPCallTarget{AgentID: input.AgentID, OperationID: input.OperationID})
	if err != nil || !reflect.DeepEqual(mcpBefore, mcpAfter) {
		t.Fatal(mcpAfter, err)
	}
	var version, count int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM plugin_calls`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestPluginSchemaMigrationRollsBack(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.db.Exec(`DROP TABLE plugin_progress; DROP TABLE plugin_calls; PRAGMA user_version=14; CREATE TABLE plugin_progress (blocker TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := s.initialize(); err == nil {
		t.Fatal("expected migration conflict")
	}
	var version, count int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 14 {
		t.Fatal(version, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='plugin_calls'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial migration", count, err)
	}
	if _, err := s.db.Exec(`DROP TABLE plugin_progress`); err != nil {
		t.Fatal(err)
	}
	if err := s.initialize(); err != nil {
		t.Fatal(err)
	}
}
