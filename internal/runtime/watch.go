package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

const (
	watchPoll        = time.Second
	watchSettle      = 750 * time.Millisecond
	watchPageLimit   = 16
	watchParallelism = 2
	watchPrepareTime = 40 * time.Second
)

// Loop diagnostics contain only host-defined error kinds. Resource preparation
// failures are retained separately by resource.status; config.inspect --live
// reports configuration preparation failures.
type WatchLoopStatus struct {
	Running  bool      `json:"running"`
	LastScan time.Time `json:"last_scan"`
	Error    string    `json:"error,omitempty"`
}

type WatchStatus struct {
	Configuration WatchLoopStatus      `json:"configuration"`
	Resources     WatchLoopStatus      `json:"resources"`
	Discovery     DiscoveryWatchStatus `json:"discovery"`
	PollMS        int64                `json:"poll_ms"`
	SettleMS      int64                `json:"settle_ms"`
	PageLimit     int                  `json:"page_limit"`
	Parallelism   int                  `json:"parallelism"`
}

func (c *Core) watchStatus() WatchStatus {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	status := c.watch
	status.Discovery.Failures = append([]DiscoveryFailure(nil), status.Discovery.Failures...)
	status.PollMS, status.SettleMS = watchPoll.Milliseconds(), watchSettle.Milliseconds()
	status.PageLimit, status.Parallelism = watchPageLimit, watchParallelism
	status.Discovery.MaxRoots, status.Discovery.MaxEntries = resource.MaxResourceDirectories, resource.MaxDiscoveryEntries
	status.Discovery.PageLimit = resource.MaxDiscoveryPage
	return status
}

func (c *Core) watchState(config, running, scanned bool, failure string) {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	status := &c.watch.Resources
	if config {
		status = &c.watch.Configuration
	}
	status.Running = running
	if scanned {
		status.LastScan, status.Error = time.Now().UTC(), failure
	}
}

func watchDelay(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func watchFailure(err error) string {
	if err == nil {
		return ""
	}
	var known *protocol.Error
	if errors.As(err, &known) {
		switch known.Data.Kind {
		case "invalid_state", "invalid_resource", "invalid_config", "resource_limit", "resource_changed", "unavailable":
			return known.Data.Kind
		}
	}
	return "unavailable"
}

func watchOperation(kind string, parts ...any) string {
	raw, _ := json.Marshal(parts)
	hash := sha256.Sum256(raw)
	return "watch_" + kind + "_" + hex.EncodeToString(hash[:])
}

func (c *Core) watchConfiguration(ctx context.Context) {
	if c.configSource == nil {
		return
	}
	c.watchState(true, true, false, "")
	defer c.watchState(true, false, false, "")
	var observed, attempted string
	var since time.Time
	for ctx.Err() == nil {
		source := c.configSource
		observation, err := resource.ObserveConfig(ctx, source.paths, source.cwd)
		c.watchState(true, true, true, watchFailure(err))
		if err != nil {
			observed = "" // Interrupted writes must settle again.
		} else if observation.Version != observed {
			observed, since = observation.Version, time.Now()
		} else if time.Since(since) >= watchSettle {
			current := c.configuration()
			generation := int64(0)
			if current.publication != nil {
				generation = current.publication.Generation
			}
			op := watchOperation("config", observation.Version, generation)
			if op != attempted && (observation.Version != current.sourceVersion || c.configError.Load() != nil) {
				err = c.publishObservedConfiguration(ctx, current, observation.Version, generation, op)
				// A moving source or publication needs a fresh observation. Other
				// failures wait for changed files or an explicit manual refresh.
				if isFailure(err, "resource_changed") || isFailure(err, "config_changed") {
					observed = ""
				} else {
					attempted = op
				}
			}
		}
		if !watchDelay(ctx, watchPoll) {
			return
		}
	}
}

func (c *Core) publishObservedConfiguration(ctx context.Context, current runtimeConfiguration, observed string, generation int64, operation string) error {
	ctx, cancel := context.WithTimeout(ctx, watchPrepareTime)
	defer cancel()
	candidate, err := c.prepareConfig(ctx)
	if err == nil {
		snapshot := candidate.Snapshot()
		if resource.ConfigSourceVersion(snapshot.Documents) != observed {
			return protocol.E("resource_changed", "configuration changed since observation")
		}
		if current.publication != nil && snapshot.Version == current.publication.Version {
			// A restored, unchanged publication may become ready again after
			// a failed startup read. Clear its diagnostic without a fake receipt.
			c.configMu.Lock()
			defer c.configMu.Unlock()
			if c.configuration().publication != current.publication {
				return protocol.E("config_changed", "configuration changed during observation")
			}
			if err = candidate.Verify(ctx); err == nil {
				c.configError.Store(nil)
				return nil
			}
			failure := configFailure(err)
			failure.Data.OperationID = operation
			c.configError.Store(failure)
			return failure
		}
		req, requestErr := protocol.NewRequest("config.refresh", operation, struct{}{})
		if requestErr != nil {
			return requestErr
		}
		_, err = c.commitConfig(ctx, req, candidate, generation)
		return err
	}
	failure := configFailure(err)
	failure.Data.OperationID = operation
	c.configMu.Lock()
	if c.configuration().publication == current.publication {
		c.configError.Store(failure)
	}
	c.configMu.Unlock()
	return failure
}

type watchedCandidate struct {
	target      store.ResourceWatchTarget
	observation resource.SourceObservation
	operation   string
}

func (c *Core) watchResources(ctx context.Context) {
	c.watchState(false, true, false, "")
	defer c.watchState(false, false, false, "")
	defer c.stopDiscovery()
	var discovery discoveryCursor
	var afterAgent, afterID string
	for ctx.Err() == nil {
		page, err := c.Store.ResourceWatchPage(ctx, afterAgent, afterID, watchPageLimit)
		failure := watchFailure(err)
		if err == nil {
			var candidates []watchedCandidate
			for _, target := range page {
				if ctx.Err() != nil {
					return
				}
				candidate, err := c.observeResource(ctx, target)
				if err != nil && failure == "" {
					failure = watchFailure(err)
				}
				if candidate != nil {
					candidates = append(candidates, *candidate)
				}
			}
			if len(candidates) > 0 {
				if !watchDelay(ctx, watchSettle) {
					return
				}
				if err := c.refreshObservedResources(ctx, candidates); err != nil && failure == "" {
					failure = watchFailure(err)
				}
			}
			if len(page) == watchPageLimit {
				last := page[len(page)-1]
				afterAgent, afterID = last.AgentID, last.Info.ID
			} else {
				afterAgent, afterID = "", ""
			}
		}
		c.watchState(false, true, true, failure)
		c.discoverResources(ctx, &discovery)
		if !watchDelay(ctx, watchPoll) {
			return
		}
	}
}

func (c *Core) observeResource(ctx context.Context, target store.ResourceWatchTarget) (*watchedCandidate, error) {
	if strings.HasPrefix(target.Info.Source, "plugin://") || (target.Update != nil && target.Update.State == "preparing") {
		return nil, nil
	}
	if target.Discovery != nil {
		current := c.configuration()
		if current.publication == nil {
			return nil, nil
		}
		configured := false
		for _, root := range current.effective.Config.ResourceDirs {
			configured = configured || root == target.Discovery.Root
		}
		if !configured {
			return nil, nil
		}
		// Failed first installs remain tied to their managed root; use the
		// current publication to fence this retry, never mutate saved metadata.
		copy := *target.Discovery
		copy.Configuration = *current.publication
		target.Discovery = &copy
	}
	observation, err := resource.ObserveSource(ctx, target.Info.Kind, target.Info.Source)
	if err != nil {
		return nil, err
	}
	operation := watchOperation("resource", target.AgentID, target.Info.ID, target.Generation, observation.Version)
	if target.Update != nil && target.Update.State == "failed" && target.Update.OperationID == operation && !isFailure(target.Update.Error, "resource_changed") {
		return nil, nil
	}
	if observation.Failure == "" && target.Generation != 0 {
		published, generation, err := c.Store.PublishedResource(ctx, protocol.ResourceTarget{AgentID: target.AgentID, ID: target.Info.ID})
		if err != nil {
			return nil, err
		}
		if generation != target.Generation {
			return nil, nil // A newer publication will be considered next pass.
		}
		if observation.Version == resource.SourceVersion(published.Info.Kind, published.Info.Source, published.Files) {
			return nil, nil
		}
	}
	return &watchedCandidate{target, observation, operation}, nil
}

func (c *Core) refreshObservedResources(ctx context.Context, candidates []watchedCandidate) error {
	// Each worker holds at most one prepared package and connection. Joining the
	// page before advancing also bounds retained candidates across a slow server.
	jobs := make(chan watchedCandidate)
	failures := make(chan error, 1)
	var workers sync.WaitGroup
	for range watchParallelism {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for candidate := range jobs {
				if err := c.refreshObservedResource(ctx, candidate); err != nil {
					select {
					case failures <- err:
					default:
					}
				}
			}
		}()
	}
	for _, candidate := range candidates {
		select {
		case jobs <- candidate:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	select {
	case err := <-failures:
		return err
	default:
		return nil
	}
}

func (c *Core) refreshObservedResource(ctx context.Context, candidate watchedCandidate) error {
	ctx, cancel := context.WithTimeout(ctx, watchPrepareTime)
	defer cancel()
	target := candidate.target
	observation, err := resource.ObserveSource(ctx, target.Info.Kind, target.Info.Source)
	if err != nil {
		return err
	}
	if observation != candidate.observation {
		return nil
	}
	req, err := protocol.NewRequest("resource.refresh", candidate.operation, protocol.ResourceTarget{AgentID: target.AgentID, ID: target.Info.ID})
	if err != nil {
		return err
	}
	// Preparation owns durable failure reporting. The expected target is checked
	// transactionally, so this scan cannot supersede a newer manual operation.
	_, err = c.prepareResourceSource(ctx, req, target.AgentID, target.Info.Kind, target.Info.Source, &target, observation.Version)
	if isFailure(err, "resource_changed") {
		return nil
	}
	return err
}
