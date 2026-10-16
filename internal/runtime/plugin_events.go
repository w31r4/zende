package runtime

import (
	"context"
	"errors"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

const MaxActivePluginSubscriptions = 64

type pluginEventWorker struct {
	cancel  context.CancelFunc
	failure *protocol.Error // guarded by pluginEventMu; retained if persistence failed
}

func (c *Core) startPluginSubscription(ctx context.Context, req protocol.Request) (store.PluginSubscriptionReceipt, error) {
	// Admission shares the call host's fault fence. Always acquire call before
	// event; workers hold only their own lock when publishing a fault.
	c.pluginCallMu.Lock()
	defer c.pluginCallMu.Unlock()
	c.pluginEventMu.Lock()
	defer c.pluginEventMu.Unlock()
	prior, err := c.Store.PriorPluginSubscription(ctx, req)
	if err != nil {
		return store.PluginSubscriptionReceipt{}, err
	}
	var input protocol.PluginSubscribe
	if err := protocol.StrictDecode(req.Params, &input); err != nil {
		return store.PluginSubscriptionReceipt{}, err
	}
	target := protocol.PluginSubscriptionTarget{AgentID: input.AgentID, SubscriptionID: req.OperationID}
	if prior != nil {
		if worker := c.pluginEvents[target]; worker != nil && worker.failure != nil {
			return store.PluginSubscriptionReceipt{}, worker.failure
		}
		return prior.Receipt, nil
	}
	if c.pluginEventsClosed {
		return store.PluginSubscriptionReceipt{}, protocol.E("unavailable", "plugin event host is closed")
	}
	for existing, worker := range c.pluginCalls {
		if existing.AgentID == input.AgentID && worker.failure != nil {
			return store.PluginSubscriptionReceipt{}, worker.failure
		}
	}
	for existing, worker := range c.pluginEvents {
		if existing.AgentID == input.AgentID && worker.failure != nil {
			return store.PluginSubscriptionReceipt{}, worker.failure
		}
	}
	if len(c.pluginEvents) >= MaxActivePluginSubscriptions {
		return store.PluginSubscriptionReceipt{}, protocol.E("resource_limit", "Core has 64 active or unpersisted plugin subscriptions; inspect or close existing subscriptions")
	}
	scope, err := c.Store.PluginSubscriptionScope(ctx, req)
	if err != nil {
		return store.PluginSubscriptionReceipt{}, err
	}
	slot, release, err := c.leasePlugin(protocol.ResourceTarget{AgentID: input.AgentID, ID: input.ResourceID})
	if err != nil {
		return store.PluginSubscriptionReceipt{}, err
	}
	sub, err := slot.instance.PrepareEvents(plugin.EventSubscribe{OperationID: req.OperationID, SubscriptionID: req.OperationID, DefinitionRef: input.DefinitionRef, Scope: scope, After: input.After, Kinds: input.Kinds})
	if err != nil {
		release()
		return store.PluginSubscriptionReceipt{}, err
	}
	intent := store.PluginSubscriptionIntent{Request: req, Binding: sub.Info(), ResourceVersion: slot.version, Generation: slot.generation}
	r, fresh, err := c.Store.BeginPluginSubscription(ctx, intent, c.InstanceID)
	if err != nil || !fresh {
		release()
		if err != nil {
			return store.PluginSubscriptionReceipt{}, err
		}
		return r.Receipt, nil
	}
	work, cancel := context.WithCancel(context.Background())
	worker := &pluginEventWorker{cancel: cancel}
	if c.pluginEvents == nil {
		c.pluginEvents = map[protocol.PluginSubscriptionTarget]*pluginEventWorker{}
	}
	c.pluginEvents[target] = worker
	c.pluginEventWorkers.Add(1)
	go c.runPluginSubscription(work, target, worker, sub, slot.instance, release)
	return r.Receipt, nil
}

func persistPluginEvent(fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fn(ctx); err != nil {
		return protocol.E("persistence_failed", "plugin subscription state could not be saved or verified; inspect its original identity")
	}
	return nil
}

func (c *Core) runPluginSubscription(ctx context.Context, target protocol.PluginSubscriptionTarget, worker *pluginEventWorker, sub *plugin.EventSubscription, instance *plugin.Instance, release func()) {
	defer c.pluginEventWorkers.Done()
	defer worker.cancel()
	defer release()
	var failure error
	func() {
		defer func() {
			if recover() != nil {
				failure = protocol.E("transport_unknown", "plugin subscription worker interrupted; original operation was not replayed")
			}
		}()
		failure = c.pumpPluginSubscription(ctx, target, sub)
	}()
	var persistence error
	if failure != nil {
		state, reason := "unknown", "transport_unknown"
		var e *protocol.Error
		if errors.As(failure, &e) && (e.Data.Kind == "persistence_failed" || e.Data.Kind == "event_unavailable" || e.Data.Kind == "resource_limit") {
			reason = e.Data.Kind
		}
		if info := sub.Info(); info.State == "prepared" || info.State == "rejected" {
			state, reason = "not_dispatched", "not_dispatched"
		}
		persistence = persistPluginEvent(func(ctx context.Context) error {
			return c.Store.FailPluginSubscription(ctx, target, c.InstanceID, state, reason)
		})
		if state == "unknown" {
			// A failed observer must not leave an untracked producer or listener
			// running. Other Agent installations own independent instances.
			_ = instance.Close()
		}
	}
	c.pluginEventMu.Lock()
	defer c.pluginEventMu.Unlock()
	if persistence != nil {
		worker.failure = protocol.E("persistence_failed", "plugin subscription outcome could not be saved; inspect its original identity before new work")
	} else {
		delete(c.pluginEvents, target)
	}
}

func (c *Core) loadPluginSubscription(target protocol.PluginSubscriptionTarget) (*store.PluginSubscriptionRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := c.Store.PluginSubscription(ctx, target)
	if err != nil || r == nil {
		return nil, protocol.E("persistence_failed", "plugin subscription record is unavailable")
	}
	return r, nil
}

func (c *Core) pumpPluginSubscription(ctx context.Context, target protocol.PluginSubscriptionTarget, sub *plugin.EventSubscription) error {
	r, err := c.loadPluginSubscription(target)
	if err != nil {
		return err
	}
	if r.Receipt.CloseOperationID != "" || ctx.Err() != nil {
		return persistPluginEvent(func(ctx context.Context) error {
			return c.Store.FailPluginSubscription(ctx, target, c.InstanceID, "not_dispatched", "not_dispatched")
		})
	}
	if err := sub.Open(ctx); err != nil {
		return err
	}
	if err := persistPluginEvent(func(ctx context.Context) error {
		return c.Store.OpenPluginSubscription(ctx, target, c.InstanceID, sub.Info())
	}); err != nil {
		return err
	}
	var shutdownDeadline time.Time
	for {
		r, err := c.loadPluginSubscription(target)
		if err != nil {
			return err
		}
		switch r.Receipt.State {
		case "closed", "not_dispatched", "unknown":
			return nil
		}
		if ctx.Err() != nil {
			if shutdownDeadline.IsZero() {
				shutdownDeadline = time.Now().Add(5 * time.Second)
			}
			if time.Now().After(shutdownDeadline) {
				return protocol.E("transport_unknown", "plugin subscription could not confirm closure during Core shutdown")
			}
			if r.Receipt.CloseOperationID == "" {
				if err := persistPluginEvent(func(ctx context.Context) error {
					return c.Store.RequestPluginSubscriptionClose(ctx, target, c.InstanceID, "core_shutdown")
				}); err != nil {
					return err
				}
				continue
			}
		}
		if r.Receipt.CloseOperationID != "" {
			err := sub.Close(context.Background(), r.Receipt.CloseOperationID)
			if pluginEventBusy(err, sub) {
				waitPluginEvent(context.Background())
				continue
			}
			if err != nil {
				return err
			}
			return persistPluginEvent(func(ctx context.Context) error {
				return c.Store.ConfirmPluginSubscriptionClosed(ctx, target, c.InstanceID, sub.Info())
			})
		}
		if r.SubscriptionHistoryFull() || r.Receipt.Direction == "plugin" && r.PluginEventReadLimit() == 0 {
			if err := persistPluginEvent(func(ctx context.Context) error {
				return c.Store.RequestPluginSubscriptionClose(ctx, target, c.InstanceID, "resource_limit")
			}); err != nil {
				return err
			}
			continue
		}
		if r.Receipt.Direction == "plugin" {
			page, err := sub.Read(ctx, r.Receipt.Cursor, r.PluginEventReadLimit(), 250)
			if pluginEventBusy(err, sub) || errors.Is(err, context.Canceled) {
				waitPluginEvent(ctx)
				continue
			}
			if err != nil {
				return err
			}
			if err := persistPluginEvent(func(ctx context.Context) error { return c.Store.AppendPluginEvents(ctx, target, c.InstanceID, page) }); err != nil {
				return err
			}
			if page.Closed {
				return nil
			}
			if len(page.Events) == 0 {
				waitPluginEvent(ctx)
			}
			continue
		}
		step, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		batch, err := c.Store.BeginPluginEventDelivery(step, target, c.InstanceID)
		cancel()
		if err != nil {
			var e *protocol.Error
			if errors.As(err, &e) && e.Data.Kind == "resource_limit" {
				// The transaction rejected the oversized source event before
				// any delivery. Close at the last confirmed cursor.
				if err := persistPluginEvent(func(ctx context.Context) error {
					return c.Store.RequestPluginSubscriptionClose(ctx, target, c.InstanceID, "resource_limit")
				}); err != nil {
					return err
				}
				continue
			}
			if errors.As(err, &e) && e.Data.Kind == "operation_conflict" {
				// Only a concurrent close is retryable. An unexpected pending
				// delivery or changed owner must not become a busy retry loop.
				latest, readErr := c.loadPluginSubscription(target)
				if readErr == nil && latest.Owner == c.InstanceID && latest.Receipt.CloseOperationID != "" && latest.Receipt.State == "closing" {
					continue
				}
			}
			return err
		}
		if batch == nil {
			waitPluginEvent(ctx)
			continue
		}
		err = sub.Deliver(ctx, *batch)
		if pluginEventBusy(err, sub) || errors.Is(err, context.Canceled) && sub.Info().State == "open" {
			if err := persistPluginEvent(func(ctx context.Context) error {
				return c.Store.RejectPluginEventDelivery(ctx, target, c.InstanceID, *batch)
			}); err != nil {
				return err
			}
			waitPluginEvent(ctx)
			continue
		}
		if err != nil {
			return err
		}
		if err := persistPluginEvent(func(ctx context.Context) error {
			return c.Store.ConfirmPluginEventDelivery(ctx, target, c.InstanceID, *batch)
		}); err != nil {
			return err
		}
	}
}

// The transport maps every unconfirmed sent mutation to transport_unknown.
// resource_limit here therefore means capacity rejected it before dispatch.
func pluginEventBusy(err error, sub *plugin.EventSubscription) bool {
	var e *protocol.Error
	return errors.As(err, &e) && e.Data.Kind == "resource_limit" && sub.Info().State == "open"
}

func waitPluginEvent(ctx context.Context) {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (c *Core) pluginSubscription(ctx context.Context, target protocol.PluginSubscriptionTarget) (store.PluginSubscriptionReceipt, error) {
	c.pluginEventMu.Lock()
	worker := c.pluginEvents[target]
	var failure *protocol.Error
	if worker != nil {
		failure = worker.failure
	}
	c.pluginEventMu.Unlock()
	if failure != nil {
		return store.PluginSubscriptionReceipt{}, failure
	}
	r, err := c.Store.PluginSubscription(ctx, target)
	if err != nil {
		return store.PluginSubscriptionReceipt{}, err
	}
	if r == nil {
		return store.PluginSubscriptionReceipt{}, protocol.E("not_found", "plugin subscription not found for this Agent")
	}
	return r.Receipt, nil
}

func (c *Core) pluginEventFailure(agent string) error {
	c.pluginEventMu.Lock()
	defer c.pluginEventMu.Unlock()
	for target, worker := range c.pluginEvents {
		if target.AgentID == agent && worker.failure != nil {
			return worker.failure
		}
	}
	return nil
}

func (c *Core) closePluginSubscriptions() {
	c.pluginEventMu.Lock()
	if c.pluginEventsClosed {
		done := c.pluginEventsDone
		c.pluginEventMu.Unlock()
		<-done
		return
	}
	c.pluginEventsClosed = true
	c.pluginEventsDone = make(chan struct{})
	for _, worker := range c.pluginEvents {
		worker.cancel()
	}
	c.pluginEventMu.Unlock()
	c.pluginEventWorkers.Wait()
	close(c.pluginEventsDone)
}
