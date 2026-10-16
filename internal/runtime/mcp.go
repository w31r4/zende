package runtime

import (
	"context"
	"sync"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/mcp"
	"zende.local/harness/internal/store"
)

// Each Agent/server slot serializes durable publication with its live pointer.
// Slow negotiation happens before this gate for explicit register/refresh.
type mcpSlot struct {
	gate       chan struct{}
	conn       *resource.MCPConnection
	pkg        resource.Package
	snapshot   *mcp.Snapshot
	generation int64
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	retired    map[*resource.MCPConnection]struct{}
}

func (s *mcpSlot) lock(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *mcpSlot) unlock() { <-s.gate }

func (c *Core) mcpSlot(target protocol.ResourceTarget) (*mcpSlot, error) {
	c.mcpMu.Lock()
	defer c.mcpMu.Unlock()
	if c.mcpClosed {
		return nil, protocol.E("unavailable", "MCP resource host is closed")
	}
	if c.mcpSlots == nil {
		c.mcpSlots = map[protocol.ResourceTarget]*mcpSlot{}
	}
	s := c.mcpSlots[target]
	if s == nil {
		s = &mcpSlot{gate: make(chan struct{}, 1)}
		c.mcpSlots[target] = s
	}
	return s, nil
}

// CloseResources is owned by the Core lifetime, not client detach. It also
// supports short-lived embedders/tests that use Handle without the scheduler.
func (c *Core) CloseResources() {
	c.closePluginCalls()
	c.closePluginSubscriptions()
	c.closePlugins()
	c.mcpMu.Lock()
	c.mcpClosed = true
	slots := make([]*mcpSlot, 0, len(c.mcpSlots))
	for _, s := range c.mcpSlots {
		slots = append(slots, s)
	}
	c.mcpMu.Unlock()
	for _, s := range slots {
		_ = s.lock(context.Background())
		if s.cancel != nil {
			s.cancel()
		}
		if s.conn != nil {
			_ = s.conn.Close()
			s.conn = nil
		}
		for conn := range s.retired {
			_ = conn.Close()
		}
		s.unlock()
		s.workers.Wait()
	}
}

func (c *Core) resourceStatus(ctx context.Context, target protocol.ResourceTarget) (protocol.ResourceStatus, error) {
	status, err := c.Store.ResourceStatus(ctx, target)
	if err == nil && status.Kind == "plugin" && status.Version != "" {
		return c.pluginStatus(target, status), nil
	}
	if err != nil || status.Kind != "mcp" || status.Version == "" {
		return status, err
	}
	c.mcpMu.Lock()
	slot := c.mcpSlots[target]
	closed := c.mcpClosed
	c.mcpMu.Unlock()
	status.State = "unavailable"
	status.Error = protocol.E("unavailable", "MCP connection is offline; search reconnects or refresh republishes")
	if slot == nil || closed {
		return status, nil
	}
	if err := slot.lock(ctx); err != nil {
		return protocol.ResourceStatus{}, err
	}
	defer slot.unlock()
	if slot.conn != nil && slot.pkg.Info.Version == status.Version {
		live := slot.conn.Status()
		status.State, status.Error = live.State, live.Error
		if status.State != "ready" && status.Error == nil {
			status.Error = protocol.E("unavailable", "MCP connection is offline; search reconnects or refresh republishes")
		}
	}
	return status, nil
}

func (c *Core) publishMCP(ctx context.Context, req protocol.Request, agent string, p resource.Package, conn *resource.MCPConnection, attempt store.ResourcePreparation) (protocol.Accepted, error) {
	target := protocol.ResourceTarget{AgentID: agent, ID: p.Info.ID}
	s, err := c.mcpSlot(target)
	if err != nil {
		conn.Close()
		return protocol.Accepted{}, err
	}
	if err := s.lock(ctx); err != nil {
		conn.Close()
		return protocol.Accepted{}, err
	}
	defer s.unlock()
	owned := false
	defer func() {
		if !owned {
			conn.Close()
		}
	}()
	if err := c.mcpOpen(); err != nil {
		return protocol.Accepted{}, err
	}
	if _, err := conn.Snapshot(); err != nil {
		return protocol.Accepted{}, err
	}
	config, err := resource.ParseMCPConfig(p.Files[0].Data)
	if err != nil {
		return protocol.Accepted{}, err
	}
	snapshot, err := mcp.Restore(ctx, p.Info.ID, config.BindingVersion(), *p.MCP)
	if err != nil {
		return protocol.Accepted{}, err
	}
	accepted, err := c.Store.PublishResourceUpdate(ctx, req, agent, p, attempt)
	if err != nil {
		return protocol.Accepted{}, err
	}
	// A concurrently retried request may already have published another
	// preparation. Never install a pointer for bytes the receipt did not accept.
	if accepted.Resource == nil || accepted.Resource.Version != p.Info.Version || accepted.ResourceGeneration < s.generation {
		return accepted, nil
	}
	c.attachMCP(target, s, p, conn, snapshot, accepted.ResourceGeneration)
	owned = true
	return accepted, nil
}

func (c *Core) mcpOpen() error {
	c.mcpMu.Lock()
	defer c.mcpMu.Unlock()
	if c.mcpClosed {
		return protocol.E("unavailable", "MCP resource host is closed")
	}
	return nil
}

func (c *Core) attachMCP(target protocol.ResourceTarget, s *mcpSlot, p resource.Package, conn *resource.MCPConnection, snapshot *mcp.Snapshot, generation int64) {
	if s.cancel != nil {
		s.cancel()
	}
	if s.conn != nil {
		old := s.conn
		if s.retired == nil {
			s.retired = map[*resource.MCPConnection]struct{}{}
		}
		s.retired[old] = struct{}{}
		old.Retire()
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			<-old.Closed()
			_ = s.lock(context.Background())
			delete(s.retired, old)
			s.unlock()
		}()
	}
	s.pkg, s.conn, s.snapshot, s.generation = p, conn, snapshot, generation
	life, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		for {
			select {
			case <-life.Done():
				return
			case <-conn.Done():
				return
			case <-conn.Updates():
				ctx, stop := context.WithTimeout(life, 5*time.Second)
				if s.lock(ctx) == nil {
					if s.conn == conn && c.mcpOpen() == nil {
						_ = c.publishMCPCatalog(ctx, target, s)
					}
					s.unlock()
				}
				stop()
			}
		}
	}()
}

// The preparation connection may have observed a newer directory. Only the
// last durably published snapshot is searchable; failed writes keep that one.
func (c *Core) publishMCPCatalog(ctx context.Context, target protocol.ResourceTarget, s *mcpSlot) error {
	snapshot, err := s.conn.Snapshot()
	if err != nil {
		return err
	}
	if snapshot.Version() == s.snapshot.Version() {
		return nil
	}
	p, err := resource.BuildMCPPackage(s.pkg.Info.Source, s.pkg.Files, snapshot.Export())
	if err != nil {
		return err
	}
	req, _ := protocol.NewRequest("resource.catalog", protocol.NewID("mcp_catalog"), target)
	accepted, err := c.Store.PublishResource(ctx, req, target.AgentID, p, s.generation)
	if err != nil {
		return err
	}
	s.pkg, s.snapshot, s.generation = p, snapshot, accepted.ResourceGeneration
	return nil
}

func (c *Core) searchMCP(ctx context.Context, p protocol.MCPSearch) (mcp.SearchResult, error) {
	search := mcp.SearchRequest{Query: p.Query, Cursor: p.Cursor, Limit: p.Limit, MaxBytes: p.MaxBytes}
	if err := search.Validate(); err != nil {
		return mcp.SearchResult{}, err
	}
	target := protocol.ResourceTarget{AgentID: p.AgentID, ID: p.ID}
	s, err := c.readyMCP(ctx, target)
	if err != nil {
		return mcp.SearchResult{}, err
	}
	defer s.unlock()
	return s.snapshot.SearchPublished(search, s.pkg.Info.Version, s.generation)
}

// readyMCP returns a locked slot with a durable catalog. Callers must unlock;
// invocation takes a lease while locked, then performs network I/O unlocked.
func (c *Core) readyMCP(ctx context.Context, target protocol.ResourceTarget) (*mcpSlot, error) {
	// Check ownership before creating an in-memory slot. Unknown IDs cannot
	// grow the host map or start a process.
	status, err := c.Store.ResourceStatus(ctx, target)
	if err != nil {
		return nil, err
	}
	if status.Kind != "mcp" || status.Version == "" {
		return nil, protocol.E("unavailable", "MCP resource has no published configuration")
	}
	s, err := c.mcpSlot(target)
	if err != nil {
		return nil, err
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			s.unlock()
		}
	}()
	if err := c.mcpOpen(); err != nil {
		return nil, err
	}
	if s.conn == nil || s.conn.Status().State != "ready" || s.pkg.Info.Version != status.Version && s.generation <= status.Generation {
		pkg, generation, err := c.Store.PublishedResource(ctx, target)
		if err != nil {
			return nil, err
		}
		config, err := resource.ParseMCPConfig(pkg.Files[0].Data)
		if err != nil {
			return nil, err
		}
		conn, err := resource.ConnectMCP(ctx, pkg.Info, config, nil)
		if err != nil {
			return nil, err
		}
		owned := false
		defer func() {
			if !owned {
				conn.Close()
			}
		}()
		snapshot, err := conn.Snapshot()
		if err != nil {
			return nil, err
		}
		updated, err := resource.BuildMCPPackage(pkg.Info.Source, pkg.Files, snapshot.Export())
		if err != nil {
			return nil, err
		}
		req, _ := protocol.NewRequest("resource.reconnect", protocol.NewID("mcp_reconnect"), target)
		accepted, err := c.Store.PublishResource(ctx, req, target.AgentID, updated, generation)
		if err != nil {
			return nil, err
		}
		c.attachMCP(target, s, updated, conn, snapshot, accepted.ResourceGeneration)
		owned = true
	}
	if err := c.publishMCPCatalog(ctx, target, s); err != nil {
		return nil, err
	}
	keep = true
	return s, nil
}
