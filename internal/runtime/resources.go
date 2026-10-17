package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

func (c *Core) registerResource(ctx context.Context, req protocol.Request, p protocol.RegisterResource) (protocol.Accepted, error) {
	if accepted, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return accepted, err
	}
	snapshot, err := c.Store.Snapshot(ctx, p.AgentID)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if p.Path == "" {
		return protocol.Accepted{}, protocol.E("invalid_params", "resource path is required")
	}
	if !filepath.IsAbs(p.Path) && !strings.HasPrefix(p.Path, "plugin://") {
		p.Path = filepath.Join(snapshot.Agent.Cwd, p.Path)
	}
	return c.prepareResource(ctx, req, p.AgentID, p.Kind, p.Path)
}

func (c *Core) refreshResource(ctx context.Context, req protocol.Request, p protocol.ResourceTarget) (protocol.Accepted, error) {
	if accepted, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return accepted, err
	}
	current, err := c.Store.ResourceStatus(ctx, p)
	if err != nil {
		return protocol.Accepted{}, err
	}
	return c.prepareResource(ctx, req, p.AgentID, current.Kind, current.Source)
}

func (c *Core) prepareResource(ctx context.Context, req protocol.Request, agent, kind, path string) (accepted protocol.Accepted, err error) {
	return c.prepareResourceSource(ctx, req, agent, kind, path, nil, "")
}

func (c *Core) prepareResourceSource(ctx context.Context, req protocol.Request, agent, kind, path string, watched *store.ResourceWatchTarget, observedVersion string) (accepted protocol.Accepted, err error) {
	identity, err := resource.Identity(kind, path)
	if err != nil {
		return protocol.Accepted{}, err
	}
	var attempt store.ResourcePreparation
	var previous *protocol.Accepted
	if watched == nil {
		attempt, previous, err = c.Store.BeginResourceUpdate(ctx, req, agent, identity, c.InstanceID)
	} else {
		attempt, previous, err = c.Store.BeginWatchedResourceUpdate(ctx, req, *watched, c.InstanceID)
	}
	if err != nil {
		return protocol.Accepted{}, err
	}
	if previous != nil {
		return *previous, nil
	}
	defer func() {
		if err == nil {
			return
		}
		reason := "internal"
		var known *protocol.Error
		if errors.As(err, &known) {
			switch known.Data.Kind {
			case "invalid_resource", "resource_limit", "resource_changed", "unavailable", "timeout", "cancelled", "plugin_protocol", "plugin_initialization", "version_mismatch", "plugin_exited":
				reason = known.Data.Kind
			}
			// Copy, never modify a shared error owned by another layer.
			copy := *known
			err = &copy
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason = "cancelled"
			err = protocol.E(reason, "resource preparation cancelled; inspect status before retrying")
		} else {
			err = protocol.E(reason, "resource publication failed; inspect status before retrying")
		}
		failure := err.(*protocol.Error)
		failure.Data.ResourceID, failure.Data.OperationID = identity.ID, req.OperationID
		// Persist failure even if this client disconnected. Bounded cleanup
		// cannot keep an IPC handler alive indefinitely during shutdown.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if saveErr := c.Store.FailResourceUpdate(cleanup, agent, attempt, reason); saveErr != nil {
			failure = protocol.E("internal", "resource update status could not be saved; inspect status before retrying")
			failure.Data.ResourceID, failure.Data.OperationID = identity.ID, req.OperationID
			err = failure
		}
	}()
	checkObservation := func(p resource.Package) error {
		if observedVersion != "" && resource.SourceVersion(p.Info.Kind, p.Info.Source, p.Files) != observedVersion {
			return protocol.E("resource_changed", "source changed since observation; observe it again")
		}
		return nil
	}
	if strings.HasPrefix(identity.Source, "plugin://") {
		return c.publishPluginResource(ctx, req, agent, identity.Source, attempt)
	}
	if kind == "mcp" {
		prepared, conn, prepareErr := resource.PrepareMCP(ctx, identity.Source, nil)
		if prepareErr != nil {
			return protocol.Accepted{}, prepareErr
		}
		if err := checkObservation(prepared); err != nil {
			conn.Close()
			return protocol.Accepted{}, err
		}
		return c.publishMCP(ctx, req, agent, prepared, conn, attempt)
	}
	if kind == "plugin" {
		prepared, instance, prepareErr := resource.PreparePluginPackage(ctx, identity.Source, nil)
		if prepareErr != nil {
			return protocol.Accepted{}, prepareErr
		}
		if err := checkObservation(prepared); err != nil {
			instance.Close()
			return protocol.Accepted{}, err
		}
		return c.publishPlugin(ctx, req, agent, prepared, instance, attempt)
	}
	prepared, err := resource.Prepare(ctx, kind, identity.Source)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if err := checkObservation(prepared); err != nil {
		return protocol.Accepted{}, err
	}
	return c.Store.PublishResourceUpdate(ctx, req, agent, prepared, attempt)
}
