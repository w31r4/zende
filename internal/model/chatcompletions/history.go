package chatcompletions

import (
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func (a *Adapter) KeepsPrivateHistory(message model.Message, modelID string) bool {
	var saved privateMessage
	return message.Protocol == Protocol && len(message.Opaque) != 0 && protocol.StrictDecode(message.Opaque, &saved) == nil &&
		saved.Version == 1 && saved.Endpoint == a.endpoint && saved.Model == modelID && saved.ReasoningField == a.config.ReasoningField
}

func ExportPublic(message model.Message) (model.PublicExport, error) {
	if err := model.ValidateAssistantExport(message); err != nil {
		return model.PublicExport{}, err
	}
	var saved privateMessage
	if message.Protocol != Protocol || len(message.Opaque) == 0 || len(message.Opaque) > model.MaxBlockBytes ||
		protocol.StrictDecode(message.Opaque, &saved) != nil || saved.Endpoint == "" || saved.Model == "" {
		return model.PublicExport{}, wireError("invalid source envelope for public conversion")
	}
	switch saved.ReasoningField {
	case "", "reasoning_content", "reasoning", "reasoning_text":
	default:
		return model.PublicExport{}, protocol.E("unsupported_capability", "Chat Completions reasoning extension cannot be converted")
	}
	source := Adapter{endpoint: saved.Endpoint, config: Config{InstructionRole: "system", MaxTokensField: "max_completion_tokens", ReasoningField: saved.ReasoningField}}
	request := model.Request{Model: saved.Model, Messages: []model.Message{message}, MaxOutputTokens: 1}
	if _, err := source.encode(request, true); err != nil {
		return model.PublicExport{}, err
	}
	if saved.Content == "" && saved.Refusal == "" && saved.Reasoning != "" {
		return model.PrivateOnlyExport(message)
	}
	// Refusal is already part of the validated public text. Reasoning is not.
	copy := model.PublicCopy(message)
	return model.PublicExport{Message: &copy}, nil
}
