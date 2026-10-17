package runtime

import (
	"context"
	"time"

	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

// A scan retains one filesystem page and advances one Agent at a time. It
// shares the resource watch worker pool instead of multiplying concurrency.
type discoveryCursor struct {
	generation int64
	root       int
	kind       int
	after      string
	page       *resource.DiscoveryPage
	afterAgent string
}

type DiscoveryWatchStatus struct {
	WatchLoopStatus
	RootCount  int                `json:"root_count"`
	Root       string             `json:"root,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	MaxRoots   int                `json:"max_roots"`
	MaxEntries int                `json:"max_entries"`
	PageLimit  int                `json:"page_limit"`
	Failures   []DiscoveryFailure `json:"failures,omitempty"`
}

type DiscoveryFailure struct {
	Root  string `json:"root,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Error string `json:"error"`
}

func (c *Core) discoveryState(count int, root, kind string, err error) {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	c.watch.Discovery.WatchLoopStatus = WatchLoopStatus{Running: true, LastScan: time.Now().UTC(), Error: watchFailure(err)}
	c.watch.Discovery.RootCount, c.watch.Discovery.Root, c.watch.Discovery.Kind = count, root, kind
	failures := c.watch.Discovery.Failures
	for i := 0; i < len(failures); i++ {
		if failures[i].Root == root && failures[i].Kind == kind {
			failures = append(failures[:i], failures[i+1:]...)
			break
		}
	}
	if err != nil {
		failures = append(failures, DiscoveryFailure{Root: root, Kind: kind, Error: watchFailure(err)})
	}
	c.watch.Discovery.Failures = failures
}

func (c *Core) resetDiscoveryFailures() {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	c.watch.Discovery.Failures = nil
}

func (c *Core) stopDiscovery() {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	c.watch.Discovery.Running = false
}

func (cursor *discoveryCursor) advance(rootCount, kindCount int) {
	cursor.afterAgent = ""
	if cursor.page != nil && cursor.page.More {
		cursor.after = cursor.page.Next
	} else {
		cursor.after = ""
		cursor.kind++
		if cursor.kind == kindCount {
			cursor.kind = 0
			cursor.root++
		}
		if cursor.root == rootCount {
			cursor.root = 0
		}
	}
	cursor.page = nil
}

func (c *Core) discoverResources(ctx context.Context, cursor *discoveryCursor) {
	if ctx.Err() != nil {
		return
	}
	current := c.configuration()
	roots := current.effective.Config.ResourceDirs
	if current.publication == nil || len(roots) == 0 {
		*cursor = discoveryCursor{}
		c.resetDiscoveryFailures()
		c.discoveryState(len(roots), "", "", nil)
		return
	}
	if cursor.generation != current.publication.Generation {
		*cursor = discoveryCursor{generation: current.publication.Generation}
		c.resetDiscoveryFailures()
	}
	if err := resource.ValidateResourceDirectories(roots); err != nil {
		c.discoveryState(len(roots), "", "", err)
		return
	}
	kinds := resource.DiscoveryKinds()
	root, kind := roots[cursor.root], kinds[cursor.kind]
	if cursor.page == nil {
		page, err := resource.DiscoverPage(ctx, root, kind, cursor.after, resource.MaxDiscoveryPage)
		if err != nil {
			c.discoveryState(len(roots), root, kind, err)
			cursor.advance(len(roots), len(kinds))
			return
		}
		cursor.page = &page
	}
	if len(cursor.page.Items) == 0 {
		cursor.advance(len(roots), len(kinds))
		c.discoveryState(len(roots), root, kind, nil)
		return
	}
	agent, targets, err := c.Store.UndiscoveredResources(ctx, cursor.afterAgent, cursor.page.Items)
	if err != nil {
		c.discoveryState(len(roots), root, kind, err)
		return
	}
	if agent == "" {
		cursor.advance(len(roots), len(kinds))
		c.discoveryState(len(roots), root, kind, nil)
		return
	}
	cursor.afterAgent = agent
	var candidates []watchedCandidate
	var firstError error
	for _, target := range targets {
		target.Discovery = &store.ResourceDiscovery{Root: root, Configuration: *current.publication}
		candidate, err := c.observeResource(ctx, target)
		if err != nil && firstError == nil {
			firstError = err
		}
		if candidate != nil {
			candidates = append(candidates, *candidate)
		}
	}
	if len(candidates) > 0 && watchDelay(ctx, watchSettle) {
		if err := c.refreshObservedResources(ctx, candidates); err != nil && firstError == nil {
			firstError = err
		}
	}
	c.discoveryState(len(roots), root, kind, firstError)
}
