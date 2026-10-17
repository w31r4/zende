package runtime

import (
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
)

// Preview and dispatch use the same versioned policy. The source remains in
// the append-only context; only this request copy is given to the adapter.
func projectRequestContext(binding model.Binding, request *model.Request) (*protocol.ContextProjection, error) {
	if binding.ContextMode == "" {
		return nil, nil
	}
	if binding.ContextMode != model.PublicContext {
		return nil, protocol.E("model_snapshot_invalid", "unsupported context conversion policy")
	}
	projection, err := provider.ProjectPublicHistory(binding, request.Messages)
	if err != nil {
		return nil, err
	}
	info := &protocol.ContextProjection{Version: 1, SourceProtocols: projection.SourceProtocols,
		Converted: projection.Converted, OmittedPrivate: projection.OmittedPrivate,
		CategoryMarkers: projection.CategoryMarkers, NativeSearchCalls: len(projection.NativeSearchCalls)}
	if !info.Valid() {
		return nil, protocol.E("context_invalid", "invalid public context projection")
	}
	request.Messages, request.NativeSearchCalls = projection.Messages, projection.NativeSearchCalls
	if len(request.NativeSearchCalls) != 0 && !nativeToolSearch(binding.ToolSearch) {
		return info, protocol.E("context_conversion_unsupported", "retained native search calls require a native search target; select a configured native route or explicitly compact the old discovery history")
	}
	return info, nil
}

func validContextSettings(settings taskSettings) bool {
	if settings.ContextMode != "" && settings.ContextMode != model.PublicContext {
		return false
	}
	if settings.ContextMode == model.PublicContext && settings.Version != 2 {
		return false
	}
	_, mode, err := model.SplitContextBinding(settings.Binding)
	return err == nil && mode == settings.ContextMode
}

func validContextProjectionRecord(record requestRecord) bool {
	if record.Settings == nil || record.Settings.ContextMode == "" {
		return record.Projection == nil
	}
	return record.Settings.ContextMode == model.PublicContext && record.Projection != nil && record.Projection.Valid() &&
		(record.Projection.NativeSearchCalls == 0 || nativeToolSearch(record.Settings.ToolSearch))
}
