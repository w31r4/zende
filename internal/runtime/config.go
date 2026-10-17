package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

// One immutable bundle prevents a new default from being resolved against an
// old catalog. Requests keep their captured bundle/binding after publication.
type runtimeConfiguration struct {
	effective     resource.Effective
	publication   *protocol.ConfigPublication
	models        model.Resolver
	restore       model.Restorer
	catalog       ModelCatalog
	defaultModel  bool
	sourceVersion string
}

type configSource struct {
	paths resource.Paths
	cwd   string
	env   func(string) string
}

type ConfigStatus struct {
	resource.Effective
	Publication *protocol.ConfigPublication `json:"publication,omitempty"`
	ProjectRoot string                      `json:"project_root,omitempty"`
	UpdateError *protocol.Error             `json:"update_error,omitempty"`
}

func (c *Core) configuration() runtimeConfiguration {
	if current := c.liveConfig.Load(); current != nil {
		return *current
	}
	resolver := c.Models
	if resolver == nil {
		resolver, _ = c.RestoreModels.(model.Resolver)
	}
	restore := c.RestoreModels
	if restore == nil {
		restore, _ = c.Models.(model.Restorer)
	}
	return runtimeConfiguration{effective: c.Config, models: resolver, restore: restore, catalog: c.Catalog, defaultModel: c.Models != nil}
}

func (c *Core) configStatus() ConfigStatus {
	c.configMu.Lock()
	current := c.configuration()
	status := ConfigStatus{Effective: current.effective, Publication: current.publication, UpdateError: c.configError.Load()}
	if c.configSource != nil {
		status.ProjectRoot = c.configSource.cwd
	}
	c.configMu.Unlock()
	// Queries must not hand mutable maps or pointers back to an in-process
	// caller. No credentials are resolved in this copy.
	raw, _ := json.Marshal(status)
	var copy ConfigStatus
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func configBundle(snapshot resource.ConfigSnapshot, publication protocol.ConfigPublication, resolver *provider.Resolver) *runtimeConfiguration {
	return &runtimeConfiguration{effective: snapshot.Effective, publication: &publication,
		models: resolver, restore: resolver, catalog: resolver, defaultModel: snapshot.Effective.Config.Model != "",
		sourceVersion: resource.ConfigSourceVersion(snapshot.Documents)}
}

func runnerConfiguration(config resource.Config, publication *protocol.ConfigPublication) process.Configuration {
	if publication != nil {
		copy := *publication
		publication = &copy
	}
	return process.Configuration{Publication: publication, Shell: config.Shell, MaxProcesses: config.Limits.Processes, MaxOutput: config.Limits.OutputBytes}
}

func (c *Core) runnerConfiguration() (process.Configuration, error) {
	current := c.configuration()
	if current.publication == nil {
		return process.Configuration{}, protocol.E("unavailable", "Core has no published runner configuration")
	}
	return runnerConfiguration(current.effective.Config, current.publication), nil
}

// InitializeConfiguration runs before serving or scheduling. Invalid current
// files keep the last committed configuration available for repair/inspection.
// Without any published version, initialization reports the actual failure.
func (c *Core) InitializeConfiguration(ctx context.Context, paths resource.Paths, cwd string, env func(string) string) error {
	if c.configSource != nil {
		return protocol.E("conflict", "configuration source was already initialized")
	}
	if env == nil {
		env = os.Getenv
	}
	c.configSource = &configSource{paths: paths, cwd: cwd, env: env}
	current, err := c.Store.CurrentConfig(ctx)
	if err != nil {
		return err
	}
	if current != nil {
		resolver, err := provider.New(current.Snapshot.Effective.Config, env)
		if err != nil {
			return protocol.E("invalid_state", "saved configuration cannot restore its model catalog")
		}
		c.liveConfig.Store(configBundle(current.Snapshot, current.Publication, resolver))
	}
	candidate, err := c.prepareConfig(ctx)
	if err == nil {
		snapshot := candidate.Snapshot()
		if current != nil && current.Snapshot.Version == snapshot.Version {
			// Existing metadata is identical. Credential/file checks succeeded;
			// restarting alone must not manufacture a new publication.
			return nil
		}
		req, requestErr := protocol.NewRequest("config.refresh", protocol.NewID("config"), struct{}{})
		if requestErr != nil {
			return requestErr
		}
		generation := int64(0)
		if current != nil {
			generation = current.Publication.Generation
		}
		_, err = c.commitConfig(ctx, req, candidate, generation)
	}
	if err != nil {
		failure := configFailure(err)
		c.configError.Store(failure)
		if current == nil || ctx.Err() != nil || isFailure(err, "persistence_failed") {
			return failure
		}
	}
	return nil
}

func (c *Core) prepareConfig(ctx context.Context) (*provider.PreparedConfig, error) {
	if c.configSource == nil {
		return nil, protocol.E("unavailable", "core has no reloadable configuration source")
	}
	source := c.configSource
	prepared, err := resource.PrepareConfig(ctx, source.paths, source.cwd, source.env, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := process.CheckConfiguration(runnerConfiguration(prepared.Snapshot().Effective.Config, nil)); err != nil {
		return nil, err
	}
	return provider.PrepareConfig(ctx, prepared, source.env)
}

func (c *Core) refreshConfig(ctx context.Context, req protocol.Request) (protocol.Accepted, error) {
	// A receipt may be returned only after its bundle is installed. The commit
	// path holds this same lock across the transaction and pointer replacement.
	c.configMu.Lock()
	old, found, err := c.Store.AcceptedRequest(ctx, req)
	current := c.configuration()
	c.configMu.Unlock()
	if found || err != nil {
		return old, err
	}
	generation := int64(0)
	if current.publication != nil {
		generation = current.publication.Generation
	}
	candidate, err := c.prepareConfig(ctx)
	if err == nil {
		return c.commitConfig(ctx, req, candidate, generation)
	}
	failure := configFailure(err)
	failure.Data.OperationID = req.OperationID
	c.configMu.Lock()
	defer c.configMu.Unlock()
	// Another caller may have committed this same operation while local
	// preparation failed. Its durable receipt takes precedence over the error.
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	// A late failure must not overwrite a newer successful publication.
	if c.configuration().publication == current.publication {
		c.configError.Store(failure)
	}
	return protocol.Accepted{}, failure
}

func (c *Core) commitConfig(ctx context.Context, req protocol.Request, candidate *provider.PreparedConfig, generation int64) (protocol.Accepted, error) {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	current := c.configuration()
	actual := int64(0)
	if current.publication != nil {
		actual = current.publication.Generation
	}
	if actual != generation {
		return protocol.Accepted{}, protocol.E("config_changed", "configuration changed during preparation; refresh again")
	}
	if err := candidate.Verify(ctx); err != nil {
		failure := configFailure(err)
		failure.Data.OperationID = req.OperationID
		c.configError.Store(failure)
		return protocol.Accepted{}, failure
	}
	snapshot := candidate.Snapshot()
	if err := process.CheckConfiguration(runnerConfiguration(snapshot.Effective.Config, nil)); err != nil {
		failure := configFailure(err)
		failure.Data.OperationID = req.OperationID
		c.configError.Store(failure)
		return protocol.Accepted{}, failure
	}
	accepted, err := c.Store.PublishConfig(ctx, req, snapshot, generation)
	if err != nil {
		failure := protocol.E("persistence_failed", "configuration publication was not confirmed; inspect the original operation before retrying")
		failure.Data.OperationID = req.OperationID
		c.configError.Store(failure)
		return protocol.Accepted{}, failure
	}
	// Store.PublishConfig always returns this committed reference. No file,
	// provider or other fallible operation may intervene after commit.
	c.liveConfig.Store(configBundle(snapshot, *accepted.Config, candidate.Resolver()))
	c.configError.Store(nil)
	return accepted, nil
}

func configFailure(err error) *protocol.Error {
	kind := "invalid_config"
	var known *protocol.Error
	if errors.As(err, &known) {
		switch known.Data.Kind {
		case "credential_unavailable", "dependency_missing", "resource_changed", "resource_limit", "unavailable", "config_changed", "persistence_failed", "cancelled":
			kind = known.Data.Kind
		}
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		kind = "cancelled"
	}
	return protocol.E(kind, "configuration refresh failed; last publication is retained; inspect local configuration and credential references")
}

func configNotice(publication *protocol.ConfigPublication, previous int64, refresh bool, commands []CommandHint) *store.ContextMessage {
	if publication == nil || !refresh && publication.Generation == previous {
		return nil
	}
	available := false
	for _, command := range commands {
		available = available || command.Name == "config.inspect"
	}
	if !available {
		return nil
	}
	raw, _ := json.Marshal(publication)
	return &store.ContextMessage{Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: "<configuration source=\"zende/config\" version=\"1\">" + string(raw) + "</configuration>\nConfiguration and model catalog were published. Discover connections with model.connections/model.list and explicitly select with agent.select. Publication does not change the current model or cwd. Task budget policy stays fixed; config.inspect reads published settings. Existing calls and processes keep their bindings."}}}}
}
