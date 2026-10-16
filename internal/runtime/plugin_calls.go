package runtime

import (
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

// Entry adapters must consult PriorPluginCall before preparation. This helper
// only acquires a published instance and validates arguments; it sends no call.
// The owner commits BeginPluginCall before Invoke and releases after retaining
// the outcome, or on every path that abandons preparation.
func (c *Core) preparePluginCall(req protocol.Request, scope plugin.Scope) (*plugin.Call, store.PluginInvocation, func(), error) {
	var input store.PluginCallInput
	var invocation store.PluginInvocation
	if req.Validate() != nil || protocol.StrictDecode(req.Params, &input) != nil || input.AgentID != scope.AgentID || input.BoundTaskID() != scope.TaskID || (req.Method != "plugin.tool.call" && req.Method != "plugin.command.call") {
		return nil, invocation, nil, protocol.E("invalid_params", "plugin request must match its host-assigned scope and callable kind")
	}
	slot, release, err := c.leasePlugin(protocol.ResourceTarget{AgentID: input.AgentID, ID: input.ResourceID})
	if err != nil {
		return nil, invocation, nil, err
	}
	call, err := slot.instance.PrepareCall(input.DefinitionRef, req.OperationID, scope, input.Arguments)
	if err != nil {
		release()
		return nil, invocation, nil, err
	}
	if req.Method != "plugin."+call.Definition().Kind+".call" {
		release()
		return nil, invocation, nil, protocol.E("invalid_params", "plugin reference has a different callable kind")
	}
	invocation = store.PluginInvocation{Request: req, Binding: call.Binding(), Scope: call.Scope(), Definition: call.Definition(), ResourceVersion: slot.version, Generation: slot.generation}
	return call, invocation, release, nil
}
