package runtime

import (
	"encoding/json"
	"fmt"
	"testing"

	"zende.local/harness/internal/protocol"
)

func TestPluginReadsValidateWithoutStartingProcesses(t *testing.T) {
	c, agent, _, _ := runtimePluginFixture(t)
	for _, method := range []string{"plugin.inspect", "plugin.progress", "plugin.result"} {
		for _, tc := range []struct{ body, kind string }{
			{`{"agent_id":%q,"operation_id":"missing"}`, "not_found"},
			{`{"agent_id":%q,"operation_id":"missing","invoke":true}`, "invalid_params"},
			{`{"agent_id":%q,"operation_id":"missing","operation_id":"other"}`, "invalid_params"},
			{`{"agent_id":%q,"operation_id":""}`, "invalid_params"},
		} {
			req, _ := protocol.NewRequest(method, "", json.RawMessage(fmt.Sprintf(tc.body, agent)))
			if _, err := c.Handle(t.Context(), req); !isFailure(err, tc.kind) {
				t.Fatal(method, tc.body, err)
			}
		}
	}
	if len(c.pluginSlots) != 0 || len(c.pluginOwned) != 0 {
		t.Fatal("read started a plugin")
	}
}
