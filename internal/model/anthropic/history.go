package anthropic

import (
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func (a *Adapter) KeepsPrivateHistory(message model.Message, modelID string) bool {
	var saved savedBlock
	return message.Protocol == Protocol && len(message.Opaque) != 0 && protocol.StrictDecode(message.Opaque, &saved) == nil &&
		(saved.Version == 1 || saved.Version == 2 && a.config.ClientToolSearch) && saved.Endpoint == a.endpoint && saved.Model == modelID
}

func ExportPublic(message model.Message) (model.PublicExport, error) {
	if err := model.ValidateAssistantExport(message); err != nil {
		return model.PublicExport{}, err
	}
	var saved savedBlock
	if message.Protocol != Protocol || len(message.Opaque) == 0 || len(message.Opaque) > model.MaxBlockBytes ||
		protocol.StrictDecode(message.Opaque, &saved) != nil || saved.Endpoint == "" || saved.Model == "" {
		return model.PublicExport{}, wireError("invalid source envelope for public conversion")
	}
	// This codec-only adapter has no client or credential callback. Thinking
	// configuration does not affect validation of the already saved blocks.
	source := Adapter{endpoint: saved.Endpoint, config: Config{ClientToolSearch: true}}
	request := model.Request{Model: saved.Model, Messages: []model.Message{message}, MaxOutputTokens: 1}
	if _, err := source.encode(request, true); err != nil {
		return model.PublicExport{}, err
	}
	var block wireBlock
	if err := decodeObject(saved.Block, &block); err != nil {
		return model.PublicExport{}, err
	}
	if block.Type == "thinking" || block.Type == "redacted_thinking" {
		return model.PrivateOnlyExport(message)
	}
	copy := model.PublicCopy(message)
	result := model.PublicExport{Message: &copy}
	if saved.Version == 2 {
		result.NativeSearchCallID = block.ID
	}
	return result, nil
}
