package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type contextBoundary struct {
	cursor    int64
	view      ContextView
	request   *protocol.ContextRequestInfo
	resources *protocol.ResourceCatalog
}

func validContextID(id string) bool {
	return id != "" && len(id) <= 512 && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}

func contextQueryBoundary(ctx context.Context, tx *sql.Tx, p protocol.ContextTarget) (contextBoundary, error) {
	var b contextBoundary
	if !validContextID(p.AgentID) || p.At != nil && *p.At < 0 || p.RequestID != "" && (p.At != nil || !validContextID(p.RequestID)) {
		return b, protocol.E("invalid_params", "context requires an Agent and either a nonnegative at cursor or a request ID")
	}
	if err := tx.QueryRowContext(ctx, "SELECT seq FROM agents WHERE id=?", p.AgentID).Scan(&b.cursor); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return b, protocol.E("not_found", "Agent not found")
		}
		return b, err
	}
	latest := b.cursor
	if p.At != nil {
		b.cursor = *p.At
	}
	var requestView string
	if p.RequestID != "" {
		var err error
		b.request, b.resources, b.cursor, requestView, err = contextRequest(ctx, tx, p.AgentID, p.RequestID)
		if err != nil {
			return b, err
		}
	}
	if b.cursor > latest {
		return b, protocol.E("cursor_gap", "context cursor is ahead of retained history")
	}
	var err error
	b.view, err = contextView(ctx, tx, p.AgentID, b.cursor)
	if err != nil {
		return b, err
	}
	if b.view.ID != "" && (b.view.AgentID != p.AgentID || b.view.Through <= 0 || b.view.Through > b.view.SourceCursor || b.view.SourceCursor >= b.view.Published || b.view.Published > b.cursor) {
		return b, protocol.E("context_invalid", "saved context view has an invalid range")
	}
	if b.request != nil && b.view.ID != requestView {
		return b, protocol.E("context_invalid", "request context view differs from its saved boundary")
	}
	return b, nil
}

func contextViewInfo(view ContextView) *protocol.ContextViewInfo {
	if view.ID == "" {
		return nil
	}
	return &protocol.ContextViewInfo{ID: view.ID, Previous: view.Previous, SourceCursor: view.SourceCursor, Through: view.Through, Published: view.Published}
}

// Parse only public request metadata. In particular, never copy settings.binding
// or plugin_binding into an export, and never resolve a credential while reading.
func contextRequest(ctx context.Context, tx *sql.Tx, agent, id string) (*protocol.ContextRequestInfo, *protocol.ResourceCatalog, int64, string, error) {
	var saved struct {
		Configuration   *protocol.ConfigPublication   `json:"configuration"`
		ID              string                        `json:"request_id"`
		Model           string                        `json:"model"`
		Protocol        string                        `json:"protocol"`
		Cursor          *int64                        `json:"context_cursor"`
		ViewID          string                        `json:"context_view"`
		Tools           []protocol.ContextToolVersion `json:"tools"`
		MaxOutputTokens int                           `json:"max_output_tokens"`
		FormatRepair    bool                          `json:"format_repair"`
		Catalog         *protocol.ResourceCatalog     `json:"resource_catalog"`
		ContextBudget   *protocol.ContextBudget       `json:"context_budget"`
		Settings        *struct {
			Configuration     *protocol.ConfigPublication `json:"configuration"`
			Selection         string                      `json:"selection"`
			Cwd               string                      `json:"cwd"`
			Effort            string                      `json:"effort"`
			SelectionRevision int64                       `json:"selection_revision"`
		} `json:"settings"`
	}
	out := &protocol.ContextRequestInfo{}
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT seq,task_id,payload FROM events WHERE agent_id=? AND kind='model.started'
 AND json_extract(payload,'$.request_id')=? ORDER BY seq LIMIT 1`, agent, id).Scan(&out.Started, &out.TaskID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, 0, "", protocol.E("not_found", "model request not found for this Agent")
	}
	if err != nil {
		return nil, nil, 0, "", err
	}
	if len(raw) > protocol.MaxFrameBytes || json.Unmarshal(raw, &saved) != nil || saved.ID != id || saved.Cursor == nil || *saved.Cursor < 0 || *saved.Cursor >= out.Started {
		return nil, nil, 0, "", protocol.E("context_invalid", "saved request boundary is invalid")
	}
	if saved.Catalog != nil && (!saved.Catalog.Valid() || saved.Catalog.Generation > *saved.Cursor) {
		return nil, nil, 0, "", protocol.E("context_invalid", "saved request resource catalog is invalid")
	}
	if saved.ContextBudget != nil && (!model.ValidContextBudget(*saved.ContextBudget) || saved.ContextBudget.OutputReserve != int64(saved.MaxOutputTokens)) {
		return nil, nil, 0, "", protocol.E("context_invalid", "saved request context budget is invalid")
	}
	out.ID, out.Model, out.Protocol = saved.ID, saved.Model, saved.Protocol
	out.ContextBudget = saved.ContextBudget
	if err := checkConfigPublication(ctx, tx, saved.Configuration); err != nil {
		return nil, nil, 0, "", err
	}
	out.Configuration = saved.Configuration
	out.MaxOutputTokens, out.FormatRepair, out.Tools = saved.MaxOutputTokens, saved.FormatRepair, saved.Tools
	if out.Tools == nil {
		out.Tools = []protocol.ContextToolVersion{}
	}
	if s := saved.Settings; s != nil {
		if err := checkConfigPublication(ctx, tx, s.Configuration); err != nil {
			return nil, nil, 0, "", err
		}
		if s.Configuration != nil && (saved.Configuration == nil || s.Configuration.Generation > saved.Configuration.Generation) {
			return nil, nil, 0, "", protocol.E("invalid_state", "request configuration predates its Task configuration")
		}
		out.TaskConfiguration = s.Configuration
		out.Selection, out.Cwd, out.Effort, out.SelectionRevision = s.Selection, s.Cwd, s.Effort, s.SelectionRevision
	}
	err = tx.QueryRowContext(ctx, `SELECT seq FROM events WHERE agent_id=? AND task_id=? AND kind='prompt.loaded' AND seq<? ORDER BY seq DESC LIMIT 1`, agent, out.TaskID, out.Started).Scan(&out.PromptSeq)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil // Older records can lack prompt/catalog metadata. Do not invent it.
	}
	return out, saved.Catalog, *saved.Cursor, saved.ViewID, err
}

func contextResourceCatalog(ctx context.Context, tx *sql.Tx, agent string, cursor int64) (*protocol.ResourceCatalog, error) {
	out := &protocol.ResourceCatalog{Entries: []protocol.ResourceRef{}}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.version,p.generation FROM resource_publications p
 JOIN (SELECT id,max(generation) AS generation FROM resource_publications WHERE agent_id=? AND generation<=? GROUP BY id) latest
 ON latest.id=p.id AND latest.generation=p.generation WHERE p.agent_id=? ORDER BY p.id LIMIT ?`, agent, cursor, agent, protocol.MaxAgentResources+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref protocol.ResourceRef
		var generation int64
		if err := rows.Scan(&ref.ID, &ref.Version, &generation); err != nil {
			return nil, err
		}
		out.Generation = max(out.Generation, generation)
		out.Entries = append(out.Entries, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !out.Valid() {
		return nil, protocol.E("context_invalid", "resource history is inconsistent")
	}
	return out, nil
}

func (s *Store) InspectContext(ctx context.Context, p protocol.ContextTarget) (protocol.ContextInfo, error) {
	var out protocol.ContextInfo
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		b, err := contextQueryBoundary(ctx, tx, p)
		if err != nil {
			return err
		}
		out.AgentID, out.Cursor, out.View, out.Request = p.AgentID, b.cursor, contextViewInfo(b.view), b.request
		out.Resources = b.resources
		if b.request == nil {
			out.Resources, err = contextResourceCatalog(ctx, tx, p.AgentID, b.cursor)
			if err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(CASE WHEN seq>? THEN 1 ELSE 0 END),0)
 FROM events WHERE agent_id=? AND kind='context.message' AND seq<=?`, b.view.Through, p.AgentID, b.cursor).Scan(&out.RawMessages, &out.Messages); err != nil {
			return err
		}
		if b.view.ID != "" {
			out.Messages++
		}
		data, err := json.Marshal(out)
		if err != nil || len(data) > 1<<20 {
			return protocol.E("context_limit", "context metadata exceeds 1 MiB")
		}
		return nil
	})
	return out, err
}

// publicContextEntry preserves committed public content and its source links.
// Provider-private bytes remain in storage for replay, outside this read API.
func publicContextEntry(seq int64, task, kind string, raw []byte) (protocol.ContextItem, error) {
	item := protocol.ContextItem{Seq: seq, TaskID: task, Kind: kind}
	if len(raw) > 32<<20 {
		return item, protocol.E("context_limit", "saved context entry exceeds the read budget")
	}
	var content any
	switch kind {
	case "context.message":
		var record ContextMessage
		if json.Unmarshal(raw, &record) != nil {
			return item, protocol.E("context_invalid", "saved context message is invalid")
		}
		item.PrivateDataBytes = len(record.Message.Opaque)
		record.Message.Opaque = nil
		content = record
	case "context.view":
		var view ContextView
		if json.Unmarshal(raw, &view) != nil || view.ID == "" {
			return item, protocol.E("context_invalid", "saved context view is invalid")
		}
		content = ContextMessage{Message: view.message(), Summary: &ContextSummaryRef{AgentID: view.AgentID, ViewID: view.ID, Through: view.Through}}
	case "prompt.loaded":
		var instruction struct{ Source, Version, Body string }
		if json.Unmarshal(raw, &instruction) != nil {
			return item, protocol.E("context_invalid", "saved prompt is invalid")
		}
		content = instruction
	default:
		return item, protocol.E("not_found", "sequence is not a context message, view or prompt")
	}
	var err error
	item.Content, err = json.Marshal(content)
	if err != nil {
		return item, protocol.E("context_invalid", "saved content could not be encoded")
	}
	item.Bytes = len(item.Content)
	digest := sha256.Sum256(item.Content)
	item.SHA256 = hex.EncodeToString(digest[:])
	return item, nil
}

func (s *Store) ReadContext(ctx context.Context, p protocol.ContextRead) (protocol.ContextPage, error) {
	out := protocol.ContextPage{AgentID: p.AgentID, Raw: p.Raw, Items: []protocol.ContextItem{}}
	if p.After < 0 || p.Limit < 0 || p.Limit > 100 || p.MaxBytes < 0 || p.MaxBytes > 4<<20 || p.MaxBytes > 0 && p.MaxBytes < 4096 || p.After > 0 && p.At == nil && p.RequestID == "" {
		return out, protocol.E("invalid_params", "context pages need a fixed at/request after the first page; limit is 1..100 and max_bytes is 4096..4194304")
	}
	if p.Limit == 0 {
		p.Limit = 20
	}
	if p.MaxBytes == 0 {
		p.MaxBytes = 256 << 10
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		b, err := contextQueryBoundary(ctx, tx, p.ContextTarget)
		if err != nil {
			return err
		}
		if p.After > b.cursor {
			return protocol.E("cursor_gap", "page cursor is ahead of the selected context")
		}
		out.Cursor, out.View, out.Next = b.cursor, contextViewInfo(b.view), p.After
		// Reserve the actual envelope with the largest possible next cursor.
		// false is one byte longer than true, so the final more flag also fits.
		envelope := out
		envelope.Next = b.cursor
		encodedEnvelope, err := json.Marshal(envelope)
		if err != nil || len(encodedEnvelope) > p.MaxBytes {
			return protocol.E("context_limit", "context metadata exceeds page budget")
		}
		used := len(encodedEnvelope)
		add := func(item protocol.ContextItem) (bool, error) {
			separator := 0
			if len(out.Items) > 0 {
				separator = 1
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				return false, err
			}
			if used+separator+len(encoded) > p.MaxBytes {
				if len(out.Items) != 0 {
					return false, nil
				}
				item.Content, item.More = nil, true
				encoded, err = json.Marshal(item)
				if err != nil || used+len(encoded) > p.MaxBytes {
					return false, protocol.E("context_limit", "context entry metadata exceeds page budget")
				}
			}
			out.Items = append(out.Items, item)
			used += separator + len(encoded)
			return true, nil
		}
		after := p.After
		if !p.Raw && b.view.ID != "" {
			after = max(after, b.view.Through)
			if p.After == 0 {
				raw, err := json.Marshal(b.view)
				if err != nil {
					return err
				}
				item, err := publicContextEntry(b.view.Published, "", "context.view", raw)
				if err != nil {
					return err
				}
				if _, err := add(item); err != nil {
					return err
				}
				out.Next = b.view.Through
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT seq,task_id,payload FROM events WHERE agent_id=? AND kind='context.message' AND seq>? AND seq<=? ORDER BY seq LIMIT ?`, p.AgentID, after, b.cursor, p.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(out.Items) == p.Limit {
				out.More = true
				break
			}
			var seq int64
			var task string
			var raw []byte
			if err := rows.Scan(&seq, &task, &raw); err != nil {
				return err
			}
			item, err := publicContextEntry(seq, task, "context.message", raw)
			if err != nil {
				return err
			}
			added, err := add(item)
			if err != nil {
				return err
			}
			if !added {
				out.More = true
				break
			}
			out.Next = seq
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !out.More {
			out.Next = b.cursor
		}
		data, err := json.Marshal(out)
		if err != nil || len(data) > p.MaxBytes {
			return protocol.E("context_limit", "context page exceeds requested byte budget")
		}
		return nil
	})
	return out, err
}

func (s *Store) ReadContextEntry(ctx context.Context, p protocol.ContextEntryRead) (protocol.ContextEntryPage, error) {
	out := protocol.ContextEntryPage{AgentID: p.AgentID, Offset: p.Offset}
	if !validContextID(p.AgentID) || p.Seq < 1 || p.Offset < 0 || p.Limit < 0 || p.Limit > 256<<10 || p.Limit > 0 && p.Limit < 4 {
		return out, protocol.E("invalid_params", "context entry requires Agent, positive seq, nonnegative offset and limit 4..262144")
	}
	if p.Limit == 0 {
		p.Limit = 64 << 10
	}
	var kind, task string
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT kind,task_id,payload FROM events WHERE agent_id=? AND seq=? AND kind IN ('context.message','context.view','prompt.loaded')`, p.AgentID, p.Seq).Scan(&kind, &task, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, protocol.E("not_found", "context entry not found for this Agent")
	}
	if err != nil {
		return out, err
	}
	item, err := publicContextEntry(p.Seq, task, kind, raw)
	if err != nil {
		return out, err
	}
	text := item.Content
	if p.Offset > len(text) || p.Offset < len(text) && !utf8.RuneStart(text[p.Offset]) {
		return out, protocol.E("invalid_cursor", "entry offset is not a UTF-8 boundary")
	}
	end := min(len(text), p.Offset+p.Limit)
	for end > p.Offset && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	out.Text, out.Next, out.More = string(text[p.Offset:end]), end, end < len(text)
	item.Content, item.More = nil, out.More
	out.Entry = item
	return out, nil
}
