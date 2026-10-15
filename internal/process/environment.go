package process

import (
	"encoding/json"
	"strings"
	"zende.local/harness/internal/protocol"
)

// Replace, never inherit, another Agent's attribution. This is public
// provenance, not a permission boundary or a secret capability.
func processEnvironment(spec Spec, inherited []string) []string {
	env := make([]string, 0, len(inherited)+1)
	for _, item := range inherited {
		if !strings.HasPrefix(item, protocol.CallerEnvironment+"=") {
			env = append(env, item)
		}
	}
	if spec.AgentID != "" && spec.TaskID != "" && spec.ID != "" {
		data, _ := json.Marshal(protocol.Caller{AgentID: spec.AgentID, TaskID: spec.TaskID, OperationID: spec.ID})
		env = append(env, protocol.CallerEnvironment+"="+string(data))
	}
	return env
}
