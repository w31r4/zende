package responses

import (
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func (a *Adapter) KeepsPrivateHistory(message model.Message, modelID string) bool {
	var saved savedItem
	return message.Protocol == Protocol && len(message.Opaque) != 0 && protocol.StrictDecode(message.Opaque, &saved) == nil &&
		(saved.Version == 1 || saved.Version == 2 && a.config.ClientToolSearch) && saved.Endpoint == a.endpoint && saved.Model == modelID
}

// ExportPublic uses the same source codec checks as continuation, with the
// recorded source scope and no HTTP client. It never invents target item IDs,
// encrypted reasoning or a replacement signature.
func ExportPublic(message model.Message) (model.PublicExport, error) {
	if err := model.ValidateAssistantExport(message); err != nil {
		return model.PublicExport{}, err
	}
	var saved savedItem
	if message.Protocol != Protocol || len(message.Opaque) == 0 || len(message.Opaque) > model.MaxBlockBytes ||
		protocol.StrictDecode(message.Opaque, &saved) != nil || saved.Endpoint == "" || saved.Model == "" {
		return model.PublicExport{}, wireError("invalid source envelope for public conversion")
	}
	request := model.Request{Model: saved.Model, Messages: []model.Message{message}, MaxOutputTokens: 1}
	if _, err := encodeSearchRequest(request, saved.Endpoint, "", true, true); err != nil {
		return model.PublicExport{}, err
	}
	var item wireItem
	if err := decodeObject(saved.Item, &item); err != nil {
		return model.PublicExport{}, err
	}
	if item.Type == "reasoning" {
		return model.PrivateOnlyExport(message)
	}
	copy := model.PublicCopy(message)
	result := model.PublicExport{Message: &copy}
	if item.Type == "tool_search_call" {
		result.NativeSearchCallID = item.CallID
	}
	return result, nil
}
