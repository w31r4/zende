package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func (c *Core) loopStatus() string {
	c.loopMu.Lock()
	defer c.loopMu.Unlock()
	if !c.modelAvailable() {
		return "unavailable"
	}
	if !c.loopRunning {
		return "stopped"
	}
	return "running"
}

// Run belongs to the elected core, never to an IPC request or TUI context.
// ClaimNext is the persistent per-agent gate; the worker set bounds goroutines.
func (c *Core) Run(ctx context.Context) error {
	c.loopMu.Lock()
	if c.loopRunning {
		c.loopMu.Unlock()
		return protocol.E("conflict", "core scheduler already running")
	}
	c.loopRunning = true
	c.loopMu.Unlock()
	defer func() { c.loopMu.Lock(); c.loopRunning = false; c.loopMu.Unlock() }()
	defer c.CloseResources()
	if err := c.Store.HoldInterrupted(ctx); err != nil {
		return err
	}
	if err := c.Store.InterruptCompactions(ctx, c.InstanceID); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if err := c.Store.InterruptResourceUpdates(ctx, c.InstanceID); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if err := c.Store.InterruptMCPCalls(ctx, c.InstanceID); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if err := c.Store.InterruptPluginCalls(ctx, c.InstanceID); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if err := c.Store.InterruptPluginSubscriptions(ctx, c.InstanceID); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer c.closePluginCalls()
	defer cancel()
	stopsDone := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); stopsDone <- c.runTreeStops(ctx) }()
	type outcome struct {
		agent string
		err   error
	}
	done := make(chan outcome, 1)
	active := map[string]bool{}
	triedRecovery := map[string]int64{}
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		configuration := c.configuration()
		maxAgents := configuration.effective.Config.Limits.Agents
		ids, err := c.Store.ReadyAgents(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		type candidate struct {
			agent   string
			held    *store.HeldTask
			compact string
		}
		var candidates []candidate
		held, err := c.Store.HeldTasks(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, h := range held {
			if seq, tried := triedRecovery[h.TaskID]; !tried || h.RequestSeq > seq {
				candidates = append(candidates, candidate{agent: h.AgentID, held: &h})
			}
		}
		compactions, err := c.Store.IdleCompactions(ctx, maxAgents)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, pending := range compactions {
			candidates = append(candidates, candidate{agent: pending.AgentID, compact: pending.OperationID})
		}
		if c.modelAvailable() {
			for _, id := range ids {
				// A configured catalog without a default may still run an
				// explicitly selected Agent. Others keep their accepted work.
				if !configuration.defaultModel {
					a, err := c.Store.Agent(ctx, id)
					if err != nil {
						if ctx.Err() != nil {
							return nil
						}
						return err
					}
					if a.Model == "" {
						continue
					}
				}
				candidates = append(candidates, candidate{agent: id})
			}
		}
		for _, next := range candidates {
			id := next.agent
			if active[id] || len(active) >= maxAgents {
				continue
			}
			if next.compact != "" {
				active[id] = true
				worker, compactCtx := c.registerCompactionWorker(ctx, id, next.compact)
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer c.forgetCompactionWorker(id, worker)
					err := c.runIdleCompaction(compactCtx, ctx, id, next.compact)
					select {
					case done <- outcome{id, err}:
					case <-ctx.Done():
					}
				}()
				continue
			}
			var work *store.Work
			var err error
			if next.held == nil {
				work, err = c.Store.ClaimNext(ctx, id)
			} else {
				work = &store.Work{Agent: protocol.Agent{ID: id}, Task: protocol.Task{ID: next.held.TaskID}}
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if work == nil {
				continue
			}
			active[id] = true
			if next.held != nil {
				triedRecovery[next.held.TaskID] = next.held.RequestSeq
			}
			taskCtx, stopTask := context.WithCancel(ctx)
			c.loopMu.Lock()
			if c.workers == nil {
				c.workers = map[string]context.CancelFunc{}
			}
			c.workers[work.Task.ID] = stopTask
			c.loopMu.Unlock()
			// A stop can commit between ClaimNext and worker registration.
			stopping, err := c.Store.CancellationRequested(ctx, work.Agent.ID, work.Task.ID)
			if err != nil {
				stopTask()
				c.forgetTask(work.Task.ID)
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if stopping {
				stopTask()
			}
			workers.Add(1)
			go func(w *store.Work) {
				defer workers.Done()
				defer stopTask()
				defer c.forgetTask(w.Task.ID)
				var err error
				if next.held != nil {
					err = c.recoverWork(taskCtx, ctx, *next.held)
				} else {
					err = c.executeWork(taskCtx, ctx, *w, nil)
				}
				select {
				case done <- outcome{w.Agent.ID, err}:
				case <-ctx.Done():
				}
			}(work)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-stopsDone:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case result := <-done:
			delete(active, result.agent)
			if result.err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return result.err
			}
		case <-tick.C:
		}
	}
}

func (c *Core) cancelTask(id string) {
	c.loopMu.Lock()
	if cancel := c.workers[id]; cancel != nil {
		cancel()
	}
	c.loopMu.Unlock()
	c.pluginCallMu.Lock()
	defer c.pluginCallMu.Unlock()
	for _, call := range c.pluginCalls {
		if call.task == id && id != "" {
			call.cancel()
		}
	}
}
func (c *Core) forgetTask(id string) {
	c.loopMu.Lock()
	defer c.loopMu.Unlock()
	delete(c.workers, id)
}

func safeFailure(err error, kind, message string) *protocol.Error {
	var known *protocol.Error
	if errors.As(err, &known) {
		return known
	}
	return protocol.E(kind, message)
}

func (c *Core) executeWork(ctx, persist context.Context, w store.Work, resume *resumedTask) (err error) {
	defer func() {
		if recover() != nil {
			err = c.Store.FinishTask(persist, w.Agent.ID, w.Task.ID, "unknown", "", protocol.E("runtime_panic", "model or tool execution stopped unexpectedly; inspect persisted calls"))
		}
	}()
	result, failure := c.runTask(ctx, persist, w, resume)
	if persist.Err() != nil {
		return nil
	} // Restart holds the unfinished task for recovery.
	state := "completed"
	var reason *protocol.Error
	if failure != nil {
		reason = safeFailure(failure, "runtime_error", "model loop failed; inspect request and tool events")
		state = "failed"
		switch reason.Data.Kind {
		case "operation_unknown", "transport_unknown", "persistence_failed", "model_dependency_unavailable":
			state = "unknown"
		}
	}
	for {
		if state != "unknown" {
			if err := c.waitPluginTask(persist, w.Agent.ID, w.Task.ID); err != nil {
				if persist.Err() != nil {
					return nil
				}
				state, result = "unknown", ""
				reason = safeFailure(err, "persistence_failed", "plugin completion could not be verified")
			}
		}
		err := c.Store.FinishTask(persist, w.Agent.ID, w.Task.ID, state, result, reason)
		var known *protocol.Error
		if errors.As(err, &known) && (known.Data.Kind == "plugin_pending" || known.Data.Kind == "operation_unknown") && state != "unknown" {
			if known.Data.Kind == "operation_unknown" {
				state, result, reason = "unknown", "", known
			}
			continue // A plugin was accepted or became unknown before commit.
		}
		return err
	}
}

type requestRecord struct {
	Configuration   *protocol.ConfigPublication `json:"configuration,omitempty"`
	ID              string                      `json:"request_id"`
	Model           string                      `json:"model"`
	Protocol        string                      `json:"protocol"`
	Cursor          int64                       `json:"context_cursor"`
	ViewID          string                      `json:"context_view,omitempty"`
	Tools           []tool.Descriptor           `json:"tools"`
	MaxOutputTokens int                         `json:"max_output_tokens"`
	FormatRepair    bool                        `json:"format_repair"`
	RepairAttempt   int                         `json:"repair_attempt"`
	RepairLimit     int                         `json:"repair_limit"`
	Settings        *taskSettings               `json:"settings,omitempty"`
	ResourceCatalog *protocol.ResourceCatalog   `json:"resource_catalog,omitempty"`
	PluginBinding   *pluginModelRequestBinding  `json:"plugin_binding,omitempty"`
	ContextBudget   *protocol.ContextBudget     `json:"context_budget,omitempty"`
}
type invocation struct {
	SchemaVersion int            `json:"schema_version,omitempty"`
	ID            string         `json:"operation_id"`
	RequestID     string         `json:"request_id"`
	Call          model.ToolCall `json:"call"`
	Version       string         `json:"version,omitempty"`
}
type toolOutcome struct {
	Invocation invocation      `json:"invocation"`
	Result     tool.Result     `json:"result"`
	Error      *protocol.Error `json:"error,omitempty"`
	// These mailbox IDs correspond, in order, to the host-added trailing
	// content blocks. Renderers show their accepted message once; models receive
	// the full tool context. Original event content remains unchanged.
	ResultMessages []string `json:"result_messages,omitempty"`
}

type taskSettings struct {
	Configuration     *protocol.ConfigPublication   `json:"configuration,omitempty"`
	Version           int                           `json:"version"`
	Selection         string                        `json:"selection"`
	Cwd               string                        `json:"cwd"`
	Model             string                        `json:"model"`
	Protocol          string                        `json:"protocol"`
	NativeCategories  bool                          `json:"native_categories"`
	Images            bool                          `json:"images,omitempty"`
	MaxOutputTokens   int                           `json:"max_output_tokens"`
	FormatRepairs     int                           `json:"format_repairs"`
	Binding           json.RawMessage               `json:"binding,omitempty"`
	Effort            string                        `json:"effort,omitempty"`
	SelectionRevision int64                         `json:"selection_revision,omitempty"`
	ModelRevision     int64                         `json:"model_revision,omitempty"`
	ContextWindow     int                           `json:"context_window,omitempty"`
	ContextPolicy     *protocol.ContextBudgetPolicy `json:"context_policy,omitempty"`
}

type resumedTask struct {
	Journal *recoveryJournal
	Binding model.Binding
}

func (c *Core) runTask(ctx, persist context.Context, w store.Work, resume *resumedTask) (string, error) {
	configuration := c.configuration()
	config := configuration.effective.Config
	var choice store.Selection
	if resume == nil {
		var err error
		choice, err = c.Store.Selection(persist, w.Agent.ID)
		if err != nil {
			return "", err
		}
		w.Agent.Cwd, w.Agent.Model = choice.Choice.Cwd, choice.Choice.Model
	}
	selection := w.Agent.Model
	if selection == "" {
		selection = config.Model
	}
	var binding model.Binding
	var err error
	if resume == nil {
		selection, binding, err = c.initialModelSelection(ctx, w.Agent.ID, choice, configuration)
		if err != nil {
			return "", safeFailure(err, "model_unavailable", "selected model is unavailable")
		}
		if binding.Adapter == nil || binding.Model == "" || binding.Protocol == "" {
			return "", protocol.E("model_unavailable", "selected model has no adapter")
		}
	} else {
		binding = resume.Binding
	}
	registry := c.Tools
	descriptors := registry.Descriptors()
	var commands []CommandHint
	for _, descriptor := range descriptors {
		if descriptor.Name == "exec_command" {
			commands = c.Commands
			break
		}
	}
	system := BuildPrompt(PromptInput{AgentID: w.Agent.ID, TaskID: w.Task.ID, Cwd: w.Agent.Cwd, Date: time.Now().Format("2006-01-02"), Tools: descriptors, Commands: commands, NativeCategories: binding.NativeCategories})
	appendEvent := func(kind string, payload any) error {
		if err := c.Store.AppendTaskEvent(persist, w.Agent.ID, w.Task.ID, kind, payload); err != nil {
			var known *protocol.Error
			if errors.As(err, &known) && known.Data.Kind == "task_cancelled" {
				return err
			}
			return protocol.E("persistence_failed", "loop event could not be saved; inspect original task")
		}
		return nil
	}
	limit := config.Limits.FormatRepairs
	policy := config.ContextBudgetPolicy()
	settings := taskSettings{Version: 2, Selection: selection, Cwd: w.Agent.Cwd, Model: binding.Model, Protocol: binding.Protocol, NativeCategories: binding.NativeCategories, Images: binding.Images, MaxOutputTokens: binding.MaxOutputTokens, FormatRepairs: limit, Binding: binding.Snapshot, SelectionRevision: choice.Choice.Revision, ModelRevision: choice.Choice.ModelRevision, Effort: choice.Choice.Effort, ContextWindow: binding.ContextWindow, ContextPolicy: &policy}
	settings.Configuration = configuration.publication
	if choice.Choice.ModelRevision == 0 && selection == config.Model {
		settings.Effort = config.Effort
	}
	if resume == nil {
		if err := appendEvent("task.config", settings); err != nil {
			return "", err
		}
		if err := appendEvent("prompt.loaded", Instruction{Source: "zende/core", Version: "22", Body: system}); err != nil {
			return "", err
		}
	} else {
		if resume.Journal.Settings != nil {
			settings = *resume.Journal.Settings
			limit = resume.Journal.Settings.FormatRepairs
		}
		if resume.Journal.Prompt.Body != "" {
			system = resume.Journal.Prompt.Body
		} else {
			if err := appendEvent("prompt.loaded", Instruction{Source: "zende/core", Version: "22", Body: system}); err != nil {
				return "", err
			}
		}
	}
	repairs, repairOnly := 0, false
	if resume != nil {
		repairs, repairOnly = resume.Journal.RepairCount, resume.Journal.RepairOnly
	}
	markInvalid := func(request string, failure error) error {
		return appendEvent("model.output.invalid", map[string]any{"request_id": request, "error": safeFailure(failure, "output_format", "incomplete or invalid visible output")})
	}
	scheduleRepair := func(request string, interrupted bool) error {
		if repairs >= limit {
			return protocol.E("output_format", "output format repair limit reached; raw output and completed tool results are preserved")
		}
		repairs++
		instruction := formatRepairInstruction(binding.NativeCategories, interrupted)
		if err := c.Store.RecordFormatRepair(persist, w.Agent.ID, w.Task.ID, request, repairs, limit, instruction); err != nil {
			var known *protocol.Error
			if errors.As(err, &known) && known.Data.Kind == "task_cancelled" {
				return err
			}
			return protocol.E("persistence_failed", "format repair could not be saved")
		}
		repairOnly = true
		return nil
	}
	if resume != nil {
		j := resume.Journal
		if j.Request != nil {
			var calls []model.ToolCall
			for _, call := range j.Calls {
				calls = append(calls, call.Call)
			}
			if err := c.settleCalls(ctx, persist, w, j.Request.ID, calls, j.Request.FormatRepair, j.Calls, appendEvent); err != nil {
				return "", err
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if !j.ResponseComplete && j.Request.FormatRepair && j.Invalid == nil && !j.RepairScheduled {
				if err := markInvalid(j.Request.ID, protocol.E("output_format", "format repair stream was interrupted by core restart")); err != nil {
					return "", err
				}
				if err := scheduleRepair(j.Request.ID, true); err != nil {
					return "", err
				}
			}
			if j.ResponseComplete && j.Invalid == nil {
				final, complete, err := (model.Response{Output: j.Output}).Final()
				if err != nil {
					return "", err
				}
				if repairOnly {
					if err := appendEvent("model.output.repaired", map[string]any{"request_id": j.Request.ID, "attempt": repairs}); err != nil {
						return "", err
					}
					repairOnly = false
				}
				if complete {
					return final, nil
				}
			} else if j.Invalid != nil && !j.RepairScheduled {
				if err := scheduleRepair(j.Request.ID, !j.ResponseComplete); err != nil {
					return "", err
				}
			}
			if !j.ResponseComplete {
				if err := appendEvent("model.interrupted", map[string]string{"request_id": j.Request.ID, "reason": "core interrupted the stream; retained deltas are not a complete response"}); err != nil {
					return "", err
				}
			}
		}
	}
	noticePending := settings.SelectionRevision > 0 && (resume == nil || resume.Journal.Request == nil)
	previousView := ""
	precedingRequest := ""
	var previousResources int64
	var previousConfiguration int64
	if resume != nil && resume.Journal.Request != nil {
		previousView = resume.Journal.Request.ViewID
		precedingRequest = resume.Journal.Request.ID
		if resume.Journal.Request.ResourceCatalog != nil {
			previousResources = resume.Journal.Request.ResourceCatalog.Generation
		}
		if resume.Journal.Request.Configuration != nil {
			previousConfiguration = resume.Journal.Request.Configuration.Generation
		}
	}
	if resume != nil {
		if err := c.Store.InterruptTaskCompaction(persist, w.Agent.ID, w.Task.ID); err != nil {
			return "", compactionStoreFailure(err)
		}
	}
	conflicts := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := c.waitForAnswers(ctx, persist, w.Agent.ID, w.Task.ID); err != nil {
			return "", err
		}
		if err := c.Store.DeliverSteer(persist, w.Agent.ID, w.Task.ID); err != nil {
			return "", safeFailure(err, "persistence_failed", "mailbox delivery could not be saved")
		}
		nextSettings, nextBinding, err := c.requestSelection(ctx, w.Agent.ID, settings, binding)
		if err != nil {
			return "", err
		}
		changed, err := c.manualCompaction(ctx, persist, w, nextSettings, nextBinding)
		if isFailure(err, "request_conflict") && conflicts < 8 {
			conflicts++
			continue
		}
		if err != nil {
			return "", err
		}
		if changed {
			continue
		}
		contextSnapshot, err := c.Store.ContextSnapshot(persist, w.Agent.ID)
		if err != nil {
			return "", protocol.E("persistence_failed", "model context could not be read")
		}
		messages, cursor := contextSnapshot.Messages, contextSnapshot.Cursor
		notice := ""
		// Compaction can cover old runtime notices. Reassert current settings
		// once per new view rather than relying on a lower-priority summary.
		if noticePending || nextSettings.SelectionRevision != settings.SelectionRevision || contextSnapshot.View.ID != previousView {
			notice = selectionNotice(nextSettings)
		}
		var notices []store.ContextMessage
		if notice != "" {
			notices = append(notices, store.ContextMessage{Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: notice}}}})
		}
		if changed := resourceNotice(contextSnapshot.Resources, previousResources, contextSnapshot.View.ID != previousView, commands); changed != nil {
			notices = append(notices, *changed)
		}
		publication := c.configuration().publication
		if changed := configNotice(publication, previousConfiguration, contextSnapshot.View.ID != previousView, commands); changed != nil {
			notices = append(notices, *changed)
		}
		for _, item := range notices {
			messages = append(messages, item.Message)
		}
		req := model.Request{ID: protocol.NewID("request"), Model: nextBinding.Model, System: system, Messages: messages, MaxOutputTokens: nextBinding.MaxOutputTokens}
		requestDescriptors := descriptors
		if repairOnly {
			requestDescriptors = nil
		}
		for _, d := range requestDescriptors {
			req.Tools = append(req.Tools, d.Tool)
		}
		var budget *protocol.ContextBudget
		if nextSettings.ContextPolicy != nil {
			estimate, err := model.EstimateRequest(req)
			if err != nil {
				return "", err
			}
			planned, err := model.PlanContextBudget(estimate, int64(nextSettings.ContextWindow), int64(req.MaxOutputTokens), *nextSettings.ContextPolicy)
			if err != nil {
				return "", err
			}
			budget = &planned
			trigger := planned.State == "compaction_needed" || planned.State == "over_capacity"
			changed, err := c.automaticCompaction(ctx, persist, w, precedingRequest, nextSettings, nextBinding, trigger)
			if isFailure(err, "request_conflict") && conflicts < 8 {
				conflicts++
				continue
			}
			if err != nil {
				return "", err
			}
			if changed {
				continue
			}
			if planned.State == "over_capacity" || planned.State == "insufficient_capacity" {
				err := c.Store.RecordContextBudgetBlock(persist, w.Agent.ID, w.Task.ID, nextSettings.SelectionRevision, cursor, contextSnapshot.View.ID, planned)
				if isFailure(err, "request_conflict") && conflicts < 8 {
					conflicts++
					continue
				}
				if err != nil {
					return "", safeFailure(err, "persistence_failed", "capacity refusal could not be saved")
				}
				failure := protocol.E("context_capacity", "selected capacity or reserve remains insufficient; history is preserved; select a larger window or adjust the next Task's budget")
				failure.Data.Retryable = true
				if planned.State == "over_capacity" {
					failure.Data.OperationID = automaticCompactionID(w.Agent.ID, w.Task.ID, precedingRequest)
				}
				return "", failure
			}
		}
		// Old image references may be covered by a successful summary. Resolve
		// the selected request only after compaction has had a chance to run.
		req.Images, err = c.resolveImages(ctx, w.Agent.ID, messages, nextBinding.Images)
		if err != nil {
			return "", err
		}
		if validator, ok := nextBinding.Adapter.(model.RequestValidator); ok {
			if err := validator.ValidateRequest(req); err != nil {
				return "", err
			}
		}
		adapter, pluginBinding, releaseModel, err := c.prepareModelRequest(nextBinding, plugin.Scope{AgentID: w.Agent.ID, TaskID: w.Task.ID, Cwd: nextSettings.Cwd}, req.ID)
		if err != nil {
			return "", modelDependencyFailure(nextBinding, err)
		}
		err = c.Store.BeginModelRequest(persist, w.Agent.ID, w.Task.ID, nextSettings.SelectionRevision, cursor, notices, func(cursor int64) any {
			return requestRecord{ID: req.ID, Model: nextBinding.Model, Protocol: nextBinding.Protocol, Cursor: cursor, ViewID: contextSnapshot.View.ID, Tools: requestDescriptors, MaxOutputTokens: req.MaxOutputTokens, FormatRepair: repairOnly, RepairAttempt: repairs, RepairLimit: limit, Settings: &nextSettings, ResourceCatalog: &contextSnapshot.Resources.Catalog, PluginBinding: pluginBinding, ContextBudget: budget, Configuration: publication}
		})
		if err != nil {
			releaseModel()
		}
		if isFailure(err, "request_conflict") && conflicts < 8 {
			conflicts++
			continue
		}
		if err != nil {
			return "", safeFailure(err, "persistence_failed", "model request could not be saved")
		}
		conflicts, noticePending = 0, false
		previousView = contextSnapshot.View.ID
		precedingRequest = req.ID
		previousResources = contextSnapshot.Resources.Catalog.Generation
		if publication != nil {
			previousConfiguration = publication.Generation
		}
		settings, binding, w.Agent.Cwd = nextSettings, nextBinding, nextSettings.Cwd
		var collector model.Collector
		classifier := model.Classifier{Native: binding.NativeCategories}
		var emitErr error
		err = adapter.Stream(ctx, req, func(delta model.Delta) error {
			if emitErr != nil {
				return emitErr
			}
			size := len(delta.Text) + len(delta.Raw) + len(delta.Name) + len(delta.CallID) + len(delta.MessageID) + len(delta.Protocol)
			if size > model.MaxBlockBytes {
				emitErr = appendEvent("model.limit", map[string]any{"request_id": req.ID, "received_bytes": size, "limit": model.MaxBlockBytes})
				if emitErr == nil {
					emitErr = protocol.E("output_limit", "model delta exceeds the storage budget; prior deltas are preserved")
				}
				return emitErr
			}
			// Preserve the received delta even if its sequence is invalid.
			var textBytes []byte
			if !utf8.ValidString(delta.Text) {
				textBytes = []byte(delta.Text)
			}
			emitErr = appendEvent("model.delta", struct {
				RequestID string      `json:"request_id"`
				Delta     model.Delta `json:"delta"`
				TextBytes []byte      `json:"text_bytes,omitempty"`
			}{req.ID, delta, textBytes})
			if emitErr == nil {
				emitErr = collector.Add(delta)
			}
			if emitErr == nil {
				// Formatting errors do not truncate the raw response or authorize
				// partial calls. The classifier keeps its first error for later.
				changes, _ := classifier.Add(delta)
				for _, change := range changes {
					emitErr = appendEvent("model.output.delta", struct {
						RequestID string            `json:"request_id"`
						Delta     model.PublicDelta `json:"delta"`
					}{req.ID, change})
					if emitErr != nil {
						break
					}
				}
			}
			if emitErr == nil {
				emitErr = ctx.Err()
			}
			return emitErr
		})
		releaseModel()
		if emitErr != nil {
			return "", emitErr
		}
		if err != nil {
			failure := safeFailure(modelDependencyFailure(binding, err), "model_stream", "model stream interrupted; received deltas are preserved")
			if ctx.Err() != nil || !classifier.HasText() {
				return "", failure
			}
			if err := markInvalid(req.ID, failure); err != nil {
				return "", err
			}
			if err := scheduleRepair(req.ID, true); err != nil {
				return "", err
			}
			continue
		}
		response, err := collector.Response()
		if err != nil {
			if !classifier.HasText() {
				return "", err
			}
			if err := markInvalid(req.ID, err); err != nil {
				return "", err
			}
			if err := scheduleRepair(req.ID, true); err != nil {
				return "", err
			}
			continue
		}
		if err := response.ValidateProtocol(binding.Protocol); err != nil {
			return "", protocol.E("model_protocol", "adapter returned another protocol's private data")
		}
		output, formatErr := classifier.Output()
		response.Output = output
		var final string
		var complete bool
		if formatErr == nil {
			final, complete, formatErr = response.Final()
		}
		if formatErr == nil && len(output) == 0 && len(response.Calls) == 0 {
			formatErr = protocol.E("output_format", "response has no explicitly categorized text or tool calls")
		}
		if repairOnly && len(response.Calls) > 0 {
			formatErr = protocol.E("output_format", "format repair attempted tool calls; they will be rejected without execution")
		}
		if formatErr != nil {
			response.Output = nil // No invalid final enters the confirmed projection.
			if err := markInvalid(req.ID, formatErr); err != nil {
				return "", err
			}
		}
		if err := c.Store.RecordResponse(persist, w.Agent.ID, w.Task.ID, req.ID, response); err != nil {
			return "", protocol.E("persistence_failed", "model response could not be saved")
		}
		if complete && formatErr == nil {
			if repairOnly {
				if err := appendEvent("model.output.repaired", map[string]any{"request_id": req.ID, "attempt": repairs}); err != nil {
					return "", err
				}
			}
			return final, nil
		}
		if err := c.settleCalls(ctx, persist, w, req.ID, response.Calls, repairOnly, nil, appendEvent); err != nil {
			return "", err
		}
		if formatErr != nil {
			if err := scheduleRepair(req.ID, false); err != nil {
				return "", err
			}
			continue
		}
		if repairOnly {
			if err := appendEvent("model.output.repaired", map[string]any{"request_id": req.ID, "attempt": repairs}); err != nil {
				return "", err
			}
			repairOnly = false
		}
	}
}

func formatRepairInstruction(native, interrupted bool) string {
	text := "The preceding response has an output-format or completeness error. For this next response only, provide explicitly categorized public text. Tools are disabled; any tool calls will be rejected without execution. Do not repeat completed tool calls. Their results remain in context. "
	if interrupted {
		text += "No tool calls from the interrupted response were executed. "
	}
	if native {
		text += "Use the protocol's commentary/final categories. "
	} else {
		text += "Use <commentary>...</commentary> or <final>...</final>, with each tag on its own line. Keep literal tags fenced or escaped. "
	}
	return text + "If more work is needed, use commentary to state the next step; normal tool access resumes after valid repair. Otherwise provide one final with no following visible content."
}
