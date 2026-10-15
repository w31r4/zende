package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

type Core struct {
	Store      *store.Store
	Config     resource.Effective
	InstanceID string
	Runner     process.Runner
	// Configure before Run; publication of hot resources is added separately.
	Models        model.Resolver
	RestoreModels model.Restorer
	Catalog       ModelCatalog
	Tools         *tool.Registry
	Commands      []CommandHint
	loopMu        sync.Mutex
	loopRunning   bool
	workers       map[string]context.CancelFunc // task ID, never just Agent ID
}

func New(s *store.Store, config resource.Effective) *Core {
	return &Core{Store: s, Config: config, InstanceID: protocol.NewID("core")}
}

func (c *Core) Handle(ctx context.Context, req protocol.Request) (any, error) {
	switch req.Method {
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
		if c.Catalog == nil {
			return nil, protocol.E("unavailable", "model catalog is not configured")
		}
		return c.Catalog.Connections(ctx, p)
	case "model.list":
		var p protocol.ModelQuery
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if c.Catalog == nil {
			return nil, protocol.E("unavailable", "model catalog is not configured")
		}
		return c.Catalog.ListModels(ctx, p)
	case "resource.register":
		var p protocol.RegisterResource
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.registerResource(ctx, req, p)
	case "resource.refresh", "resource.status":
		var p protocol.ResourceTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if req.Method == "resource.refresh" {
			return c.refreshResource(ctx, req, p)
		}
		return c.Store.ResourceStatus(ctx, p)
	case "resource.list":
		var p protocol.ResourceList
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Store.Resources(ctx, p)
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
		}{c.InstanceID, os.Getpid(), c.Config.Config.Limits, c.loopStatus()}, nil
	case "config.inspect":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return c.Config, nil
	case "agent.create":
		var p protocol.CreateAgent
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if p.Cwd == "" {
			p.Cwd = c.Config.Config.Cwd
		}
		if !filepath.IsAbs(p.Cwd) {
			return nil, protocol.E("invalid_params", "cwd must be absolute")
		}
		st, err := os.Stat(p.Cwd)
		if err != nil || !st.IsDir() {
			return nil, protocol.E("invalid_params", "cwd is not an accessible directory")
		}
		return c.Store.Create(ctx, req, p, c.Config.Config.Limits.Agents)
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
		return c.Store.Submit(ctx, req, p, c.Config.Config.Limits.Agents)
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
	case "agent.stop", "agent.resume":
		var p protocol.Target
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if req.Method == "agent.resume" {
			return c.Store.Resume(ctx, req, p, c.Config.Config.Limits.Agents)
		}
		accepted, err := c.Store.Stop(ctx, req, p)
		if err == nil {
			c.cancelTask(accepted.TaskID)
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
