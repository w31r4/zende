package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

type Core struct {
	Store        *store.Store
	Config       resource.Effective
	InstanceID   string
	Runner       process.Runner
	configMu     sync.Mutex
	configSource *configSource // Set once before serving/scheduling.
	liveConfig   atomic.Pointer[runtimeConfiguration]
	configError  atomic.Pointer[protocol.Error]
	watchMu      sync.Mutex
	watch        WatchStatus
	// Legacy injection for embedders; configure before Run. Live configuration
	// uses one immutable bundle without mutating these fields.
	Models             model.Resolver
	RestoreModels      model.Restorer
	Catalog            ModelCatalog
	Tools              *tool.Registry
	Commands           []CommandHint
	loopMu             sync.Mutex
	loopRunning        bool
	workers            map[string]context.CancelFunc // task ID, never just Agent ID
	compactionWorkers  map[string]*compactionWorker  // idle summaries, keyed by Agent
	mcpMu              sync.Mutex
	mcpSlots           map[protocol.ResourceTarget]*mcpSlot
	mcpClosed          bool
	pluginMu           sync.Mutex
	pluginSlots        map[protocol.ResourceTarget]*pluginSlot
	pluginOwned        map[*pluginSlot]protocol.ResourceTarget
	pluginCleanup      sync.WaitGroup
	pluginClosed       chan struct{}
	pluginsClosed      bool
	pluginCallMu       sync.Mutex
	pluginCalls        map[protocol.PluginCallTarget]*pluginExecution
	pluginCallWorkers  sync.WaitGroup
	pluginCallsClosed  bool
	pluginCallsDone    chan struct{}
	pluginEventMu      sync.Mutex
	pluginEvents       map[protocol.PluginSubscriptionTarget]*pluginEventWorker
	pluginEventWorkers sync.WaitGroup
	pluginEventsClosed bool
	pluginEventsDone   chan struct{}
}

func New(s *store.Store, config resource.Effective) *Core {
	return &Core{Store: s, Config: config, InstanceID: protocol.NewID("core")}
}

func (c *Core) Handle(ctx context.Context, req protocol.Request) (any, error) {
	config := c.configuration().effective.Config
	switch req.Method {
	case "config.runner":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.runnerConfiguration()
	case "config.refresh":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.refreshConfig(ctx, req)
	case "config.read":
		var p protocol.ConfigRead
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ConfigVersion(ctx, p.Version)
	case "config.history":
		var p protocol.ConfigQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ConfigHistory(ctx, p.After, p.Limit)
	case "context.compact":
		var p protocol.CompactContext
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.QueueCompaction(ctx, req, p.AgentID, "", "manual", config.ContextBudgetPolicy())
	case "context.inspect":
		var p protocol.ContextTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.InspectContext(ctx, p)
	case "context.read":
		var p protocol.ContextRead
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ReadContext(ctx, p)
	case "context.entry":
		var p protocol.ContextEntryRead
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ReadContextEntry(ctx, p)
	case "agent.wait":
		var p protocol.WaitTask
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.waitTask(ctx, p)
	case "agent.result":
		var p protocol.ReadTaskResult
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ReadTaskResult(ctx, p)
	case "agent.send":
		var p protocol.SendMessage
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Send(ctx, req, p, config.Limits.Agents)
	case "mailbox.inspect":
		var p protocol.MailTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Mail(ctx, p.ID)
	case "mailbox.list":
		var p protocol.MailboxQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Mailbox(ctx, p)
	case "agent.selection":
		var p protocol.Target
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		s, err := c.Store.Selection(ctx, p.AgentID)
		return s.Choice, err
	case "agent.select":
		var p protocol.SelectAgent
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.selectAgent(ctx, req, p)
	case "agent.select.preview":
		var p protocol.SelectAgent
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.previewSelection(ctx, p)
	case "workspace.directory":
		var p protocol.DirectoryQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.directory(ctx, p)
	case "workspace.git":
		var p protocol.GitQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.gitStatus(ctx, p)
	case "model.connections":
		var p protocol.ConnectionQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.modelConnections(ctx, p)
	case "model.list":
		var p protocol.ModelQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.modelList(ctx, p)
	case "resource.register":
		var p protocol.RegisterResource
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.registerResource(ctx, req, p)
	case "resource.watch.status":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.watchStatus(), nil
	case "resource.refresh", "resource.status":
		var p protocol.ResourceTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if req.Method == "resource.refresh" {
			return c.refreshResource(ctx, req, p)
		}
		return c.resourceStatus(ctx, p)
	case "mcp.search":
		var p protocol.MCPSearch
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.searchMCP(ctx, p)
	case "plugin.catalog":
		var p protocol.PluginCatalogQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.PluginCatalog(ctx, p)
	case "plugin.events.subscribe":
		return c.startPluginSubscription(ctx, req)
	case "plugin.events.close":
		return c.Store.ClosePluginSubscription(ctx, req)
	case "plugin.events.inspect":
		var p protocol.PluginSubscriptionTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.pluginSubscription(ctx, p)
	case "plugin.events.list":
		var p protocol.PluginSubscriptionQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.PluginSubscriptions(ctx, p)
	case "plugin.events.read":
		var p protocol.PluginEventsRead
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ReadPluginEvents(ctx, p)
	case "plugin.instances":
		var p protocol.ResourceTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.pluginInstances(ctx, p)
	case "plugin.inspect":
		var p protocol.PluginCallTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if err := c.pluginCallFailure(p); err != nil {
			return nil, err
		}
		record, err := c.Store.PluginCall(ctx, p)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, protocol.E("not_found", "plugin call not found for this Agent")
		}
		return record.Receipt, nil
	case "plugin.command.call":
		// Public clients cannot assign a Task or cwd. Caller provenance is
		// resolved from the original exec_command and checked at acceptance.
		var p protocol.PluginCommand
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.startPluginCall(ctx, req, plugin.Scope{})
	case "plugin.cancel":
		return c.cancelPluginCall(ctx, req)
	case "plugin.progress":
		var p protocol.PluginProgressQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.PluginProgress(ctx, p.PluginCallTarget, p.After, p.Limit)
	case "plugin.result":
		var p protocol.PluginResultRead
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ReadPluginResult(ctx, p.PluginCallTarget, p.Offset, p.Limit)
	case "resource.list":
		var p protocol.ResourceList
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Resources(ctx, p)
	case "mcp.inspect":
		var p protocol.MCPCallTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		record, err := c.Store.MCPCall(ctx, p)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, protocol.E("not_found", "MCP invocation was not found")
		}
		return record.Receipt, nil
	case "mcp.result":
		var p protocol.MCPResultRead
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ReadMCPResult(ctx, p)
	case "resource.read":
		var p protocol.ResourceRead
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ReadResource(ctx, p)
	case "process.inspect":
		var p process.Target
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if c.Runner == nil {
			return nil, protocol.E("unavailable", "runner is not configured")
		}
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return c.Runner.Inspect(queryCtx, p.ID)
	case "core.status":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return struct {
			InstanceID string          `json:"instance_id"`
			PID        int             `json:"pid"`
			Limits     resource.Limits `json:"limits"`
			ModelLoop  string          `json:"model_loop"`
		}{c.InstanceID, os.Getpid(), config.Limits, c.loopStatus()}, nil
	case "config.inspect":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.configStatus(), nil
	case "agent.create":
		var p protocol.CreateAgent
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if p.Cwd == "" {
			p.Cwd = config.Cwd
		}
		if !filepath.IsAbs(p.Cwd) {
			return nil, protocol.E("invalid_params", "cwd must be absolute")
		}
		st, err := os.Stat(p.Cwd)
		if err != nil || !st.IsDir() {
			return nil, protocol.E("invalid_params", "cwd is not an accessible directory")
		}
		return c.Store.Create(ctx, req, p, config.Limits.Agents)
	case "agent.spawn":
		var p protocol.SpawnAgent
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.spawnAgent(ctx, req, p)
	case "agent.submit":
		var p protocol.Submit
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if previous, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
			return previous, err
		}
		if len(p.Attachments) > 0 {
			if err := c.checkImageCapability(ctx, p.AgentID); err != nil {
				return nil, err
			}
		}
		return c.Store.Submit(ctx, req, p, config.Limits.Agents)
	case "question.ask":
		var p protocol.AskQuestion
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.AskQuestion(ctx, req, p)
	case "question.answer":
		var p protocol.AnswerQuestion
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.AnswerQuestion(ctx, req, p)
	case "question.inspect":
		var p protocol.QuestionTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Question(ctx, p)
	case "question.list":
		var p protocol.QuestionList
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Questions(ctx, p)
	case "attachment.import":
		var p protocol.ImportAttachment
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.importImage(ctx, req, p)
	case "attachment.inspect", "attachment.read":
		var p protocol.AttachmentTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if req.Method == "attachment.read" {
			return c.Store.ReadImage(ctx, p)
		}
		return c.Store.ImageInfo(ctx, p)
	case "attachment.list":
		var p protocol.AttachmentList
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Images(ctx, p)
	case "agent.recover":
		var p protocol.RecoveryTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.RequestRecovery(ctx, req, p)
	case "agent.resume":
		var p protocol.Target
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Resume(ctx, req, p, config.Limits.Agents)
	case "agent.stop":
		var p protocol.StopAgent
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if p.Tree {
			return c.beginTreeStop(ctx, req, p.AgentID)
		}
		accepted, err := c.Store.Stop(ctx, req, protocol.Target{AgentID: p.AgentID})
		if err == nil {
			c.cancelTask(accepted.TaskID)
			c.cancelIdleCompaction(ctx, p.AgentID)
		}
		return accepted, err
	case "agent.inspect":
		var p protocol.Target
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Snapshot(ctx, p.AgentID)
	case "agent.activity":
		var p protocol.Target
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.activity(ctx, p.AgentID)
	case "agent.recovery":
		var p protocol.RecoveryTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Recovery(ctx, p)
	case "agent.list":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.ListAgents(ctx)
	case "agent.history", "events.watch":
		var p protocol.Cursor
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Events(ctx, p)
	case "operation.inspect":
		var p struct {
			ID string `json:"operation_id"`
		}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		accepted, err := c.Store.Operation(ctx, p.ID)
		if err == nil {
			compaction, compactErr := c.Store.Compaction(ctx, p.ID)
			if compactErr == nil {
				return compaction.CompactionInfo, nil
			}
			if !isFailure(compactErr, "not_found") {
				return nil, compactErr
			}
			stop, stopErr := c.Store.TreeStop(ctx, p.ID)
			if stopErr == nil {
				return stop, nil
			}
			if !isFailure(stopErr, "not_found") {
				return nil, stopErr
			}
		}
		if isFailure(err, "not_found") {
			pending, queryErr := c.Store.WorktreeOperation(ctx, p.ID)
			if queryErr != nil {
				return nil, queryErr
			}
			if pending != nil {
				return pending, nil
			}
		}
		return accepted, err
	default:
		return nil, protocol.E("method_not_found", "unknown method; consult CLI --help")
	}
}

func (c *Core) Subscribe(ctx context.Context, req protocol.Request, emit func(protocol.Event) error) error {
	var cursor protocol.Cursor
	if err := protocol.StrictDecode(req.Params, &cursor); err != nil {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		page, err := c.Store.Events(ctx, cursor)
		if err != nil {
			return err
		}
		for _, event := range page.Events {
			if err := emit(event); err != nil {
				return err
			}
			cursor.After = event.Seq
		}
		if cursor.After < page.Latest {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
