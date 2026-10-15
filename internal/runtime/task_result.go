package runtime

import (
	"context"
	"encoding/json"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func (c *Core) waitTask(ctx context.Context, p protocol.WaitTask) (protocol.TaskOutcome, error) {
	deadline := time.NewTimer(time.Duration(p.WaitMS) * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		out, err := c.Store.ObserveTask(ctx, p)
		if err != nil || out.Terminal {
			return out, err
		}
		if p.WaitMS == 0 {
			out.TimedOut = true
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-deadline.C:
			out.TimedOut = true
			return out, nil
		case <-tick.C:
		}
	}
}

func (c *Core) attachWaitResults(ctx context.Context, agent string, call model.ToolCall, result tool.Result) (tool.Result, store.ToolMailDelivery, error) {
	d := store.ToolMailDelivery{}
	if result.ProcessID == "" || (call.Name != "exec_command" && call.Name != "write_stdin") {
		return result, d, nil
	}
	mail, err := c.Store.WaitedResults(ctx, agent, result.ProcessID)
	if err != nil {
		return result, d, err
	}
	d.ProcessID = result.ProcessID
	for _, m := range mail {
		block := model.Block{Type: "text", Text: protocol.MailResultText(m)}
		candidate := result
		candidate.Content = append(append([]model.Block(nil), result.Content...), block)
		raw, err := json.Marshal(candidate)
		if err != nil {
			return result, d, err
		}
		// Keep room for call IDs and a possible error block. Large results stay
		// in the mailbox; a CLI reference alone does not consume them.
		if len(raw) > model.MaxBlockBytes-16384 {
			continue
		}
		result = candidate
		d.Messages = append(d.Messages, m.ID)
	}
	return result, d, nil
}
