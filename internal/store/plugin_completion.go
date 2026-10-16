package store

import (
	"context"
	"database/sql"
	"reflect"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

// PluginCompletion returns only validated capability output. Raw transport
// evidence stays behind the explicit result reader, not in model context.
func (s *Store) PluginCompletion(ctx context.Context, target PluginCallTarget) (*plugin.Completion, error) {
	var completion *plugin.Completion
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := pluginRecord(ctx, tx, target)
		if err != nil {
			return err
		}
		if r == nil {
			return protocol.E("not_found", "plugin call not found for this Agent")
		}
		if r.Receipt.State != "completed" && r.Receipt.State != "failed" && r.Receipt.State != "cancelled" {
			return protocol.E("operation_unknown", "plugin call has no verified completion")
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT result FROM plugin_calls WHERE operation_id=?`, target.OperationID).Scan(&raw); err != nil {
			return err
		}
		var result plugin.CallResult
		if len(raw) != r.Receipt.ResultBytes || len(raw) > maxPluginResultBytes || pluginDigest([]byte(raw)) != r.Receipt.ResultSHA256 || protocol.StrictDecode([]byte(raw), &result) != nil || result.CallBinding != r.Invocation.Binding || result.RequestID != r.Receipt.RequestID || result.State != r.Receipt.State || result.Completion == nil {
			return protocol.E("invalid_state", "retained plugin completion failed integrity verification")
		}
		catalog, err := publishedPluginCall(ctx, tx, r.Invocation)
		if err != nil {
			return err
		}
		verified, err := catalog.ValidateCompletion(result.DefinitionRef, result.OperationID, result.Evidence)
		if err != nil || !reflect.DeepEqual(verified, *result.Completion) || verified.State != r.Receipt.State {
			return protocol.E("invalid_state", "retained plugin completion differs from original schema or evidence")
		}
		completion = &verified
		return nil
	})
	return completion, err
}
