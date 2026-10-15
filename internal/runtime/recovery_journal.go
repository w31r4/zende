package runtime

import (
	"context"
	"path/filepath"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

// The journal is a projection of append-only facts, not a second execution log.
// It retains the current request only after proving prior calls have results.
type recoveryCall struct {
	Call                              model.ToolCall
	Invocation                        *invocation
	State                             string
	IntentSeq, DispatchSeq, ResultSeq int64
	ResultPaired                      bool
	ProcessID                         string
	Failure                           *protocol.Error
	UnknownRecorded                   bool
}
type recoveryJournal struct {
	Work                     store.Work
	Request                  *requestRecord
	ResponseComplete         bool
	Output                   []model.PublicMessage
	Invalid                  *protocol.Error
	Calls                    []*recoveryCall
	byCall                   map[string]*recoveryCall
	byOperation              map[string]*recoveryCall
	RepairCount, RepairLimit int
	RepairOnly               bool
	RepairScheduled          bool
	Settings                 *taskSettings
	Prompt                   Instruction
}

type RecoveryCallInfo struct {
	CallID      string `json:"call_id"`
	Name        string `json:"name"`
	OperationID string `json:"operation_id,omitempty"`
	Version     string `json:"version,omitempty"`
	State       string `json:"state"`
	IntentSeq   int64  `json:"intent_seq,omitempty"`
	DispatchSeq int64  `json:"dispatch_seq,omitempty"`
	ResultSeq   int64  `json:"result_seq,omitempty"`
	ProcessID   string `json:"process_id,omitempty"`
}
type RecoveryReport struct {
	AgentID          string             `json:"agent_id"`
	TaskID           string             `json:"task_id"`
	TaskState        string             `json:"task_state"`
	Paused           bool               `json:"paused"`
	Cursor           int64              `json:"cursor"`
	RequestID        string             `json:"request_id,omitempty"`
	ResponseComplete bool               `json:"response_complete"`
	RepairCount      int                `json:"repair_count"`
	NextStep         string             `json:"next_step"`
	Calls            []RecoveryCallInfo `json:"calls"`
}

func recoveryInvalid() error {
	return protocol.E("recovery_invalid", "task lifecycle records are inconsistent; original history is preserved")
}

func readRecoveryJournal(ctx context.Context, s *store.Store, agent, task string) (*recoveryJournal, error) {
	j := &recoveryJournal{}
	w, err := s.TaskJournal(ctx, agent, task, j.add)
	if err != nil {
		return nil, err
	}
	j.Work = w
	for _, call := range j.Calls {
		if call.State == "completed" && !call.ResultPaired {
			return nil, recoveryInvalid()
		}
	}
	return j, nil
}

func (j *recoveryJournal) pending() bool {
	for _, c := range j.Calls {
		if c.State != "completed" || !c.ResultPaired {
			return true
		}
	}
	return false
}
func sameCall(a, b model.ToolCall) bool {
	if a.ID != b.ID || a.Name != b.Name {
		return false
	}
	x := protocol.Request{Version: 1, Method: "arguments", Params: a.Arguments}
	y := protocol.Request{Version: 1, Method: "arguments", Params: b.Arguments}
	xh, xe := x.Fingerprint()
	yh, ye := y.Fingerprint()
	return xe == nil && ye == nil && xh == yh
}
func (j *recoveryJournal) identify(op invocation) (*recoveryCall, error) {
	if j.Request == nil || !j.ResponseComplete || op.ID == "" || op.RequestID != j.Request.ID || op.SchemaVersion < 0 || op.SchemaVersion > 2 {
		return nil, recoveryInvalid()
	}
	c := j.byCall[op.Call.ID]
	if c == nil || !sameCall(c.Call, op.Call) || c.State == "completed" {
		return nil, recoveryInvalid()
	}
	if old := j.byOperation[op.ID]; old != nil && old != c {
		return nil, recoveryInvalid()
	}
	if c.Invocation != nil && (c.Invocation.ID != op.ID || c.Invocation.Version != op.Version || c.Invocation.SchemaVersion != op.SchemaVersion) {
		return nil, recoveryInvalid()
	}
	if c.Invocation == nil {
		copy := op
		c.Invocation = &copy
		j.byOperation[op.ID] = c
	}
	return c, nil
}

func (j *recoveryJournal) add(e protocol.Event) error {
	if len(e.Payload) > protocol.MaxFrameBytes {
		return protocol.E("recovery_limit", "lifecycle record exceeds recovery budget; original history is preserved")
	}
	switch e.Kind {
	case "task.config":
		var settings taskSettings
		if j.Settings != nil || protocol.StrictDecode(e.Payload, &settings) != nil || settings.Version != 1 || !filepath.IsAbs(settings.Cwd) || settings.Model == "" || settings.Protocol == "" || settings.FormatRepairs < 0 {
			return recoveryInvalid()
		}
		j.Settings = &settings
	case "prompt.loaded":
		if protocol.StrictDecode(e.Payload, &j.Prompt) != nil {
			return recoveryInvalid()
		}
	case "model.started":
		if j.pending() {
			return recoveryInvalid()
		}
		var r requestRecord
		if protocol.StrictDecode(e.Payload, &r) != nil || r.ID == "" || r.RepairAttempt != j.RepairCount || r.RepairLimit < 0 || r.FormatRepair != j.RepairOnly {
			return recoveryInvalid()
		}
		j.Request = &r
		j.ResponseComplete = false
		j.Output = nil
		j.Invalid = nil
		j.RepairScheduled = false
		j.Calls = nil
		j.byCall = map[string]*recoveryCall{}
		j.byOperation = map[string]*recoveryCall{}
		j.RepairLimit = r.RepairLimit
	case "context.message":
		var r store.ContextMessage
		if protocol.StrictDecode(e.Payload, &r) != nil {
			return recoveryInvalid()
		}
		if r.Message.Role != "assistant" && r.Message.Role != "tool" {
			return nil
		}
		if j.Request == nil || r.RequestID != j.Request.ID {
			return recoveryInvalid()
		}
		if r.Message.Role == "assistant" {
			if j.ResponseComplete {
				return recoveryInvalid()
			}
			for _, b := range r.Message.Content {
				if b.Type == "tool_call" {
					if b.CallID == "" || b.Name == "" || j.byCall[b.CallID] != nil || len(j.Calls) >= model.MaxResponseBlocks {
						return recoveryInvalid()
					}
					var args map[string]any
					if protocol.StrictDecode(b.Arguments, &args) != nil || args == nil {
						return recoveryInvalid()
					}
					call := &recoveryCall{Call: model.ToolCall{ID: b.CallID, Name: b.Name, Arguments: b.Arguments}, State: "planned"}
					j.Calls = append(j.Calls, call)
					j.byCall[b.CallID] = call
				}
			}
		} else {
			if !j.ResponseComplete || len(r.Message.Content) == 0 {
				return recoveryInvalid()
			}
			id := r.Message.Content[0].CallID
			c := j.byCall[id]
			if c == nil || c.State != "completed" || c.ResultPaired {
				return recoveryInvalid()
			}
			for _, b := range r.Message.Content {
				if b.CallID != id {
					return recoveryInvalid()
				}
			}
			c.ResultPaired = true
		}
	case "model.completed":
		var r struct {
			RequestID string                `json:"request_id"`
			Usage     *model.Usage          `json:"usage,omitempty"`
			Output    []model.PublicMessage `json:"output,omitempty"`
		}
		if protocol.StrictDecode(e.Payload, &r) != nil || j.Request == nil || r.RequestID != j.Request.ID || j.ResponseComplete {
			return recoveryInvalid()
		}
		j.ResponseComplete = true
		j.Output = r.Output
		response := model.Response{Output: r.Output}
		for _, c := range j.Calls {
			response.Calls = append(response.Calls, c.Call)
		}
		if _, _, err := response.Final(); err != nil {
			return recoveryInvalid()
		}
	case "model.output.invalid":
		var r struct {
			RequestID string          `json:"request_id"`
			Error     *protocol.Error `json:"error"`
		}
		if protocol.StrictDecode(e.Payload, &r) != nil || j.Request == nil || r.RequestID != j.Request.ID || r.Error == nil {
			return recoveryInvalid()
		}
		j.Invalid = r.Error
	case "model.repair":
		var r struct {
			RequestID string `json:"request_id"`
			Attempt   int    `json:"attempt"`
			Limit     int    `json:"limit"`
		}
		if protocol.StrictDecode(e.Payload, &r) != nil || j.Request == nil || r.RequestID != j.Request.ID || r.Attempt != j.RepairCount+1 || r.Attempt > r.Limit {
			return recoveryInvalid()
		}
		j.RepairCount = r.Attempt
		j.RepairLimit = r.Limit
		j.RepairOnly = true
		j.RepairScheduled = true
	case "model.output.repaired":
		var r struct {
			RequestID string `json:"request_id"`
			Attempt   int    `json:"attempt"`
		}
		if protocol.StrictDecode(e.Payload, &r) != nil || j.Request == nil || r.RequestID != j.Request.ID || r.Attempt != j.RepairCount || !j.RepairOnly || !j.ResponseComplete || j.Invalid != nil || j.pending() {
			return recoveryInvalid()
		}
		j.RepairOnly = false
	case "tool.started", "tool.dispatching", "tool.rejected", "tool.skipped":
		var op invocation
		if protocol.StrictDecode(e.Payload, &op) != nil {
			return recoveryInvalid()
		}
		c, err := j.identify(op)
		if err != nil {
			return err
		}
		switch e.Kind {
		case "tool.started":
			if c.State != "planned" {
				return recoveryInvalid()
			}
			c.IntentSeq = e.Seq
			c.State = "prepared"
			if op.SchemaVersion < 2 {
				c.State = "unknown"
			}
		case "tool.dispatching":
			if c.State != "prepared" || op.SchemaVersion != 2 {
				return recoveryInvalid()
			}
			c.DispatchSeq = e.Seq
			c.State = "dispatched"
		case "tool.rejected", "tool.skipped":
			legacyPrepared := c.State == "unknown" && op.SchemaVersion < 2 && !c.UnknownRecorded
			if c.State != "planned" && c.State != "prepared" && c.State != "skipped" && !legacyPrepared {
				return recoveryInvalid()
			}
			c.State = "skipped"
			c.Failure = protocol.E("task_cancelled", "tool was not executed: task cancellation was requested")
			if e.Kind == "tool.rejected" {
				c.Failure = protocol.E("output_repair_tool_forbidden", "tool was not executed: format repair accepts categorized text only")
			}
			if c.IntentSeq == 0 {
				c.IntentSeq = e.Seq
			}
		}
	case "tool.completed":
		var outcome toolOutcome
		if protocol.StrictDecode(e.Payload, &outcome) != nil {
			return recoveryInvalid()
		}
		c, err := j.identify(outcome.Invocation)
		if err != nil {
			return err
		}
		if c.State == "planned" {
			return recoveryInvalid()
		}
		c.State = "completed"
		c.ResultSeq = e.Seq
		c.ProcessID = outcome.Result.ProcessID
		c.Failure = outcome.Error
	case "tool.unknown":
		var r struct {
			Invocation  *invocation     `json:"invocation"`
			OperationID string          `json:"operation_id"`
			Result      *tool.Result    `json:"result"`
			Error       *protocol.Error `json:"error"`
			Reason      string          `json:"reason"`
			Limit       int             `json:"limit"`
		}
		if protocol.StrictDecode(e.Payload, &r) != nil {
			return recoveryInvalid()
		}
		var c *recoveryCall
		if r.Invocation != nil {
			var err error
			c, err = j.identify(*r.Invocation)
			if err != nil {
				return err
			}
		} else {
			c = j.byOperation[r.OperationID]
		}
		if c == nil || c.State == "completed" {
			return recoveryInvalid()
		}
		c.State = "unknown"
		c.Failure = r.Error
		c.UnknownRecorded = true
		if r.Result != nil {
			c.ProcessID = r.Result.ProcessID
		}
	}
	return nil
}

func (j *recoveryJournal) report() RecoveryReport {
	r := RecoveryReport{AgentID: j.Work.Agent.ID, TaskID: j.Work.Task.ID, TaskState: j.Work.Task.State, Paused: j.Work.Agent.Paused, Cursor: j.Work.Agent.Seq, ResponseComplete: j.ResponseComplete, RepairCount: j.RepairCount, Calls: []RecoveryCallInfo{}, NextStep: "continue_model"}
	if j.Request != nil {
		r.RequestID = j.Request.ID
	}
	for _, c := range j.Calls {
		info := RecoveryCallInfo{CallID: c.Call.ID, Name: c.Call.Name, State: c.State, IntentSeq: c.IntentSeq, DispatchSeq: c.DispatchSeq, ResultSeq: c.ResultSeq, ProcessID: c.ProcessID}
		if c.Invocation != nil {
			info.OperationID = c.Invocation.ID
			info.Version = c.Invocation.Version
		}
		r.Calls = append(r.Calls, info)
		if c.State != "completed" && r.NextStep != "verify" {
			r.NextStep = "resume_calls"
		}
		if c.State == "dispatched" || c.State == "unknown" {
			r.NextStep = "verify"
		}
	}
	if !j.pending() {
		if j.Work.Task.State == "cancelling" {
			r.NextStep = "settle_cancellation"
		} else if j.ResponseComplete {
			_, final, err := (model.Response{Output: j.Output}).Final()
			if err == nil && final {
				r.NextStep = "finish"
			} else if j.Invalid != nil {
				r.NextStep = "repair_output"
			}
		}
	}
	switch j.Work.Task.State {
	case "accepted":
		r.NextStep = "queued"
	case "completed", "failed", "cancelled":
		r.NextStep = "terminal"
	case "running", "cancelling":
		if j.Work.Agent.State != "unknown" {
			r.NextStep = "observe"
		}
	}
	return r
}

func (c *Core) Recovery(ctx context.Context, target protocol.RecoveryTarget) (RecoveryReport, error) {
	j, err := readRecoveryJournal(ctx, c.Store, target.AgentID, target.TaskID)
	if err != nil {
		return RecoveryReport{}, err
	}
	return j.report(), nil
}
