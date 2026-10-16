package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
)

const MaxPluginProgressEntries = 1024
const MaxPluginProgressBytes = 8 << 20
const maxPluginIntentBytes = 2 << 20
const maxPluginResultBytes = 2*plugin.MaxResultBytes + (64 << 10)

const pluginCallSchema = `
CREATE TABLE plugin_calls (
 operation_id TEXT PRIMARY KEY REFERENCES operations(id) DEFERRABLE INITIALLY DEFERRED, agent_id TEXT NOT NULL REFERENCES agents(id),
 task_id TEXT REFERENCES tasks(id), owner TEXT NOT NULL, intent TEXT NOT NULL, intent_hash TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('dispatched','cancelling','completed','failed','cancelled','not_dispatched','unknown')),
 request_id TEXT NOT NULL DEFAULT '', progress_seq INTEGER NOT NULL DEFAULT 0, progress_bytes INTEGER NOT NULL DEFAULT 0,
 result TEXT NOT NULL DEFAULT '', result_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX plugin_calls_agent ON plugin_calls(agent_id,operation_id);
CREATE TABLE plugin_progress (
 operation_id TEXT NOT NULL REFERENCES plugin_calls(operation_id), sequence INTEGER NOT NULL,
 payload TEXT NOT NULL, digest TEXT NOT NULL, PRIMARY KEY(operation_id,sequence)
);
CREATE TRIGGER plugin_intent_immutable BEFORE UPDATE OF operation_id,agent_id,task_id,owner,intent,intent_hash ON plugin_calls
 BEGIN SELECT RAISE(ABORT,'immutable plugin dispatch'); END;
CREATE TRIGGER plugin_result_immutable BEFORE UPDATE ON plugin_calls WHEN OLD.state NOT IN ('dispatched','cancelling')
 BEGIN SELECT RAISE(ABORT,'immutable plugin result'); END;
CREATE TRIGGER plugin_call_retained BEFORE DELETE ON plugin_calls
 BEGIN SELECT RAISE(ABORT,'retained plugin call'); END;
CREATE TRIGGER plugin_progress_no_update BEFORE UPDATE ON plugin_progress
 BEGIN SELECT RAISE(ABORT,'append-only plugin progress'); END;
CREATE TRIGGER plugin_progress_no_delete BEFORE DELETE ON plugin_progress
 BEGIN SELECT RAISE(ABORT,'retained plugin progress'); END;
PRAGMA user_version=15;
`

// PluginCallInput is the effect identity, independent of the selected live
// instance. TaskID is assigned/verified by the entry adapter, not plugin code.
type PluginCallInput struct {
	AgentID       string          `json:"agent_id"`
	TaskID        string          `json:"task_id,omitempty"`
	ResourceID    string          `json:"resource_id"`
	DefinitionRef string          `json:"definition_ref"`
	Arguments     json.RawMessage `json:"arguments"`
}

type PluginInvocation struct {
	Request         protocol.Request   `json:"request"`
	Binding         plugin.CallBinding `json:"binding"`
	Scope           plugin.Scope       `json:"scope"`
	Definition      plugin.Definition  `json:"definition"`
	ResourceVersion string             `json:"resource_version"`
	Generation      int64              `json:"generation"`
}

type PluginCallTarget = protocol.PluginCallTarget

// Receipts/events contain only identity and sizes. Plugin values and arbitrary
// error text remain behind the explicit progress/result readers.
type PluginReceipt struct {
	PluginCallTarget
	TaskID          string             `json:"task_id,omitempty"`
	ResourceID      string             `json:"resource_id"`
	ResourceVersion string             `json:"resource_version"`
	Generation      int64              `json:"generation"`
	Binding         plugin.CallBinding `json:"binding"`
	Kind            string             `json:"kind"`
	Name            string             `json:"name"`
	State           string             `json:"state"`
	RequestID       string             `json:"request_id,omitempty"`
	ProgressSeq     int64              `json:"progress_seq"`
	ProgressBytes   int64              `json:"progress_bytes"`
	ResultBytes     int                `json:"result_bytes"`
	ResultSHA256    string             `json:"result_sha256,omitempty"`
	Error           *protocol.Error    `json:"error,omitempty"`
}

type PluginRecord struct {
	Invocation PluginInvocation
	Owner      string
	Receipt    PluginReceipt
}

func pluginDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func pluginID(id string, required bool) bool {
	return (!required || id != "") && len(id) <= 128 && utf8.ValidString(id) && strings.IndexFunc(id, func(r rune) bool { return r < 33 || r == 127 }) < 0
}

func pluginInput(req protocol.Request) (PluginCallInput, error) {
	var p PluginCallInput
	if req.Validate() != nil || !pluginID(req.OperationID, true) || len(req.Params) > plugin.MaxArgumentBytes+(16<<10) || (req.Method != "plugin.tool.call" && req.Method != "plugin.command.call") || protocol.StrictDecode(req.Params, &p) != nil || !pluginID(p.AgentID, true) || !pluginID(p.TaskID, false) || !pluginID(p.ResourceID, true) || !pluginID(p.DefinitionRef, true) || len(p.Arguments) == 0 || len(p.Arguments) > plugin.MaxArgumentBytes {
		return p, protocol.E("invalid_params", "invalid plugin invocation identity or arguments")
	}
	return p, nil
}

func pluginStateError(state string) *protocol.Error {
	switch state {
	case "unknown":
		return protocol.E("operation_unknown", "plugin outcome is unverified; inspect the original operation, do not repeat it")
	case "failed":
		return protocol.E("plugin_call_failed", "plugin reported failure; read the retained result")
	case "not_dispatched":
		return protocol.E("plugin_not_dispatched", "plugin call was not sent; inspect the original operation")
	}
	return nil
}

func pluginRecord(ctx context.Context, q rowQuerier, target PluginCallTarget) (*PluginRecord, error) {
	var raw, digest, state, request, resultHash, task, owner string
	var progressSeq, progressBytes int64
	var resultBytes int
	err := q.QueryRowContext(ctx, `SELECT intent,intent_hash,state,request_id,coalesce(task_id,''),owner,progress_seq,progress_bytes,length(CAST(result AS BLOB)),result_hash FROM plugin_calls WHERE agent_id=? AND operation_id=?`, target.AgentID, target.OperationID).Scan(&raw, &digest, &state, &request, &task, &owner, &progressSeq, &progressBytes, &resultBytes, &resultHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var invocation PluginInvocation
	if len(raw) > maxPluginIntentBytes || pluginDigest([]byte(raw)) != digest || protocol.StrictDecode([]byte(raw), &invocation) != nil {
		return nil, protocol.E("invalid_state", "saved plugin dispatch is unreadable")
	}
	p, err := pluginInput(invocation.Request)
	b := invocation.Binding
	if err != nil || p.AgentID != target.AgentID || p.TaskID != task || invocation.Request.OperationID != target.OperationID || b.OperationID != target.OperationID || b.DefinitionRef != p.DefinitionRef || invocation.Scope.AgentID != p.AgentID || invocation.Scope.TaskID != p.TaskID || !pluginID(owner, true) || !pluginID(request, false) || progressSeq < 0 || progressSeq > MaxPluginProgressEntries || progressBytes < 0 || progressBytes > MaxPluginProgressBytes || resultBytes > maxPluginResultBytes {
		return nil, protocol.E("invalid_state", "saved plugin dispatch identity changed")
	}
	switch state {
	case "dispatched", "cancelling":
		if resultBytes != 0 || resultHash != "" {
			return nil, protocol.E("invalid_state", "pending plugin call has a result")
		}
	case "completed", "failed", "cancelled", "not_dispatched":
		if resultBytes == 0 || len(resultHash) != 64 {
			return nil, protocol.E("invalid_state", "plugin outcome has no retained result")
		}
	case "unknown":
		if (resultBytes == 0) != (resultHash == "") || resultBytes > 0 && len(resultHash) != 64 {
			return nil, protocol.E("invalid_state", "plugin evidence metadata changed")
		}
	default:
		return nil, protocol.E("invalid_state", "unknown plugin call state")
	}
	return &PluginRecord{Invocation: invocation, Owner: owner, Receipt: PluginReceipt{PluginCallTarget: target, TaskID: task, ResourceID: p.ResourceID, ResourceVersion: invocation.ResourceVersion, Generation: invocation.Generation, Binding: b, Kind: invocation.Definition.Kind, Name: invocation.Definition.Name, State: state, RequestID: request, ProgressSeq: progressSeq, ProgressBytes: progressBytes, ResultBytes: resultBytes, ResultSHA256: resultHash, Error: pluginStateError(state)}}, nil
}

func (s *Store) PluginCall(ctx context.Context, target PluginCallTarget) (*PluginRecord, error) {
	if !pluginID(target.AgentID, true) || !pluginID(target.OperationID, true) {
		return nil, protocol.E("invalid_params", "Agent and plugin operation IDs are required")
	}
	return pluginRecord(ctx, s.db, target)
}

// PriorPluginCall is consulted before discovery, reconnect or any new side
// effect. Even unknown and not-dispatched operations retain their identities.
func (s *Store) PriorPluginCall(ctx context.Context, req protocol.Request) (*PluginRecord, error) {
	p, err := pluginInput(req)
	if err != nil {
		return nil, err
	}
	_, found, err := s.AcceptedRequest(ctx, req)
	if err != nil || !found {
		return nil, err
	}
	record, err := s.PluginCall(ctx, PluginCallTarget{AgentID: p.AgentID, OperationID: req.OperationID})
	if err == nil && record == nil {
		return nil, protocol.E("invalid_state", "accepted plugin call has no dispatch record")
	}
	return record, err
}

// Validate the selected immutable publication, never the current live pointer.
func publishedPluginCall(ctx context.Context, q rowQuerier, i PluginInvocation) (*plugin.Catalog, error) {
	p, err := pluginInput(i.Request)
	if err != nil {
		return nil, err
	}
	var raw string
	err = q.QueryRowContext(ctx, `SELECT v.package FROM resource_versions v JOIN resource_publications p ON p.id=v.id AND p.version=v.version WHERE p.agent_id=? AND p.id=? AND p.version=? AND p.generation=? LIMIT 1`, p.AgentID, p.ResourceID, i.ResourceVersion, i.Generation).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, protocol.E("stale_reference", "plugin binding was not published to this Agent; discover again")
	}
	if err != nil {
		return nil, err
	}
	pkg, err := decodePackage(raw)
	if err != nil {
		return nil, err
	}
	if pkg.Plugin == nil {
		return nil, protocol.E("invalid_state", "selected resource is not a plugin")
	}
	manifest, err := resource.ParsePluginManifest(pkg.Files[0].Data)
	if err != nil {
		return nil, err
	}
	b := i.Binding
	if b.OperationID != i.Request.OperationID || b.PluginID != manifest.ID || b.PluginVersion != manifest.Version || b.ManifestVersion != manifest.Digest() || b.CatalogVersion != pkg.Plugin.CatalogVersion || b.DefinitionRef != p.DefinitionRef || !pluginID(b.InstanceID, true) || i.Scope.AgentID != p.AgentID || i.Scope.TaskID != p.TaskID || !filepath.IsAbs(i.Scope.Cwd) || filepath.Clean(i.Scope.Cwd) != i.Scope.Cwd || len(i.Scope.Cwd) > 4096 || !utf8.ValidString(i.Scope.Cwd) || strings.ContainsRune(i.Scope.Cwd, 0) || i.Request.Method != "plugin."+i.Definition.Kind+".call" {
		return nil, protocol.E("invalid_state", "plugin binding differs from its published definition or scope")
	}
	for _, d := range pkg.Plugin.Definitions {
		if reflect.DeepEqual(d, i.Definition) && d.Ref == p.DefinitionRef {
			catalog, err := plugin.RestoreCatalog(manifest, *pkg.Plugin)
			if err != nil {
				return nil, err
			}
			return catalog, catalog.ValidateArguments(p.DefinitionRef, p.Arguments)
		}
	}
	return nil, protocol.E("stale_reference", "plugin definition differs from publication; discover again")
}

// BeginPluginCall grants dispatch once, after both the acceptance and binding
// have committed. The caller must hold the instance until FinishPluginCall.
func (s *Store) BeginPluginCall(ctx context.Context, i PluginInvocation, owner string) (*PluginRecord, bool, error) {
	p, err := pluginInput(i.Request)
	if err != nil {
		return nil, false, err
	}
	if !pluginID(owner, true) {
		return nil, false, protocol.E("invalid_params", "plugin call owner is required")
	}
	raw, err := json.Marshal(i)
	if err != nil || len(raw) > maxPluginIntentBytes {
		return nil, false, protocol.E("invalid_params", "plugin dispatch exceeds the retained intent budget")
	}
	fresh := false
	_, err = s.accept(ctx, i.Request, func(tx *sql.Tx) (protocol.Accepted, error) {
		if err := resourceAgent(ctx, tx, p.AgentID, true); err != nil {
			return protocol.Accepted{}, err
		}
		var unresolved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM plugin_calls WHERE agent_id=? AND state='unknown')`, p.AgentID).Scan(&unresolved); err != nil {
			return protocol.Accepted{}, err
		}
		if unresolved {
			return protocol.Accepted{}, protocol.E("operation_unknown", "an earlier plugin outcome is unverified; inspect the original operation before new calls")
		}
		if p.TaskID != "" {
			if err := activeTask(ctx, tx, p.AgentID, p.TaskID); err != nil {
				return protocol.Accepted{}, err
			}
		}
		if _, err := publishedPluginCall(ctx, tx, i); err != nil {
			return protocol.Accepted{}, err
		}
		// The FK to operations is deferred until the enclosing accept transaction
		// inserts its receipt. Both rows must become visible together.
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_calls(operation_id,agent_id,task_id,owner,intent,intent_hash,state) VALUES(?,?,nullif(?,''),?,?,?,'dispatched')`, i.Request.OperationID, p.AgentID, p.TaskID, owner, string(raw), pluginDigest(raw)); err != nil {
			return protocol.Accepted{}, err
		}
		record, err := pluginRecord(ctx, tx, PluginCallTarget{AgentID: p.AgentID, OperationID: i.Request.OperationID})
		if err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, p.AgentID, p.TaskID, "plugin.dispatched", record.Receipt); err != nil {
			return protocol.Accepted{}, err
		}
		fresh = true
		return protocol.Accepted{AgentID: p.AgentID, TaskID: p.TaskID}, nil
	})
	if err != nil {
		return nil, false, err
	}
	record, err := s.PluginCall(ctx, PluginCallTarget{AgentID: p.AgentID, OperationID: i.Request.OperationID})
	if err == nil && record == nil {
		err = protocol.E("invalid_state", "accepted plugin call has no dispatch record")
	}
	return record, fresh && err == nil, err
}
