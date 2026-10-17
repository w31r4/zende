package gemini

import (
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func (a *Adapter) KeepsPrivateHistory(message model.Message, modelID string) bool {
	var saved savedParts
	return message.Protocol == Protocol && len(message.Opaque) != 0 && protocol.StrictDecode(message.Opaque, &saved) == nil &&
		saved.Version == 1 && saved.Endpoint == a.endpoint && saved.Model == modelID
}

func ExportPublic(message model.Message) (model.PublicExport, error) {
	if err := model.ValidateAssistantExport(message); err != nil {
		return model.PublicExport{}, err
	}
	var saved savedParts
	if message.Protocol != Protocol || len(message.Opaque) == 0 || len(message.Opaque) > model.MaxBlockBytes ||
		protocol.StrictDecode(message.Opaque, &saved) != nil || saved.Endpoint == "" || saved.Model == "" {
		return model.PublicExport{}, wireError("invalid source envelope for public conversion")
	}
	source := Adapter{endpoint: saved.Endpoint}
	request := model.Request{Model: saved.Model, Messages: []model.Message{message}, MaxOutputTokens: 1}
	if _, err := source.encode(request, true); err != nil {
		return model.PublicExport{}, err
	}
	visible, private := false, false
	for _, raw := range saved.Parts {
		p, err := decodePart(raw)
		if err != nil {
			return model.PublicExport{}, err
		}
		visible = visible || p.Call != nil || p.Text != nil && !p.Thought && *p.Text != ""
		private = private || p.Thought || p.Signature != ""
	}
	if !visible && private {
		return model.PrivateOnlyExport(message)
	}
	copy := model.PublicCopy(message)
	return model.PublicExport{Message: &copy}, nil
}
