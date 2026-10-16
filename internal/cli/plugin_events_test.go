package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

func checkPluginCLIEvents(t *testing.T, ctx context.Context, s *store.Store, source string, run func(bool, ...string) []byte) {
	t.Helper()
	t.Setenv(protocol.CallerEnvironment, "")
	t.Setenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT", filepath.Join(t.TempDir(), "events.jsonl"))
	var agent, publication protocol.Accepted
	if err := json.Unmarshal(run(true, "agent", "create", "--cwd", filepath.Dir(source)), &agent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run(true, "resource", "register", agent.AgentID, source, "--kind", "plugin"), &publication); err != nil {
		t.Fatal(err)
	}
	var catalog resource.PluginCatalog
	if err := json.Unmarshal(run(true, "plugin", "catalog", agent.AgentID, publication.Resource.ID), &catalog); err != nil || len(catalog.Events) != 2 {
		t.Fatal(catalog, err)
	}
	input := protocol.Submit{AgentID: agent.AgentID, Input: "subscribe to plugin events"}
	req, _ := protocol.NewRequest("agent.submit", "event-task", input)
	accepted, err := s.Submit(ctx, req, input, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNext(ctx, agent.AgentID); err != nil {
		t.Fatal(err)
	}
	caller := protocol.Caller{AgentID: agent.AgentID, TaskID: accepted.TaskID, OperationID: "event-shell"}
	if err := s.AppendTaskEvent(ctx, agent.AgentID, accepted.TaskID, "model.started", map[string]any{"request_id": "event-model", "settings": map[string]any{"cwd": filepath.Dir(source)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTaskEvent(ctx, agent.AgentID, accepted.TaskID, "tool.dispatching", map[string]any{"operation_id": caller.OperationID, "request_id": "event-model", "call": model.ToolCall{ID: "event-call", Name: "exec_command"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(caller)
	t.Setenv(protocol.CallerEnvironment, string(raw))
	var saved []store.PluginSubscriptionReceipt
	for _, def := range catalog.Events {
		args := []string{"plugin", "events", "subscribe", agent.AgentID, publication.Resource.ID, def.Ref}
		if def.Direction == "core" {
			args = append(args, "--kind", "agent.created", "--kind", "task.started")
		}
		var first, retry store.PluginSubscriptionReceipt
		if err := json.Unmarshal(run(true, args...), &first); err != nil || first.State != "opening" || first.Source == nil || first.Source.Caller != caller || first.Scope.TaskID != caller.TaskID {
			t.Fatal(first, err)
		}
		if err := json.Unmarshal(run(true, args...), &retry); err != nil || retry.SubscriptionID != first.SubscriptionID || retry.InstanceID != first.InstanceID {
			t.Fatal("caller-local retry changed subscription", retry, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := json.Unmarshal(run(true, "plugin", "events", "inspect", agent.AgentID, first.SubscriptionID), &retry); err != nil {
				t.Fatal(err)
			}
			if retry.State == "open" && (def.Direction == "plugin" && retry.Cursor == 3 || def.Direction == "core" && retry.Delivery != nil && retry.Delivery.Confirmed) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("subscription not ready", retry)
			}
			time.Sleep(5 * time.Millisecond)
		}
		saved = append(saved, retry)
	}
	t.Setenv(protocol.CallerEnvironment, "")
	for _, sub := range saved {
		args := []string{"--request-id", sub.SubscriptionID, "plugin", "events", "subscribe", agent.AgentID, publication.Resource.ID, sub.DefinitionRef}
		for _, kind := range sub.Kinds {
			args = append(args, "--kind", kind)
		}
		var retry store.PluginSubscriptionReceipt
		if err := json.Unmarshal(run(true, args...), &retry); err != nil || !reflect.DeepEqual(retry.Source, sub.Source) || retry.InstanceID != sub.InstanceID {
			t.Fatal("human retry lost original provenance", retry, err)
		}
		closeArgs := []string{"--request-id", "close-" + sub.SubscriptionID, "plugin", "events", "close", agent.AgentID, sub.SubscriptionID}
		first := run(true, closeArgs...)
		if !bytes.Equal(first, run(true, closeArgs...)) {
			t.Fatal("close retry changed receipt")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := json.Unmarshal(run(true, "plugin", "events", "inspect", agent.AgentID, sub.SubscriptionID), &retry); err != nil {
				t.Fatal(err)
			}
			if retry.State == "closed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("subscription not closed", retry)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if sub.Direction == "plugin" {
			var page store.PluginEventsPage
			if err := json.Unmarshal(run(true, "plugin", "events", "read", agent.AgentID, sub.SubscriptionID, "--limit", "2"), &page); err != nil || len(page.Events) != 2 || !page.More || page.Next != 2 || page.Subscription.State != "closed" || page.Subscription.ResourceVersion != publication.Resource.Version {
				t.Fatal(page, err)
			}
			if err := json.Unmarshal(run(true, "plugin", "events", "read", agent.AgentID, sub.SubscriptionID, "--after", "2"), &page); err != nil || len(page.Events) != 1 || page.More || page.Next != 3 {
				t.Fatal(page, err)
			}
		}
		for _, bad := range [][]string{{"inspect", "other-agent", sub.SubscriptionID}, {"read", agent.AgentID, sub.SubscriptionID, "--limit", "65"}, {"list", agent.AgentID, "--limit", "101"}} {
			run(false, append([]string{"plugin", "events"}, bad...)...)
		}
	}
	var page store.PluginSubscriptionsPage
	if err := json.Unmarshal(run(true, "plugin", "events", "list", agent.AgentID, "--resource", publication.Resource.ID, "--limit", "1"), &page); err != nil || !page.More || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	if err := json.Unmarshal(run(true, "plugin", "events", "list", agent.AgentID, "--after", page.Next, "--limit", "1"), &page); err != nil || page.More || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	audit, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT"))
	if err != nil || bytes.Count(audit, []byte(`"method":"plugin.events.open"`)) != 2 || bytes.Count(audit, []byte(`"method":"plugin.events.close"`)) != 2 {
		t.Fatal("CLI replayed event mutation", string(audit), err)
	}
	for _, sub := range saved {
		run(true, "plugin", "events", "inspect", agent.AgentID, sub.SubscriptionID)
	}
	after, _ := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT"))
	if !bytes.Equal(audit, after) {
		t.Fatal("saved read contacted plugin")
	}
}

func TestPluginEventsHelpAndReadOnlyClassification(t *testing.T) {
	for _, action := range []string{"subscribe", "inspect", "list", "read", "close"} {
		var out, diagnostic bytes.Buffer
		code := Run(t.Context(), []string{"plugin", "events", action, "--help"}, nil, &out, &diagnostic, BuildInfo{})
		if code != 0 || diagnostic.Len() != 0 || !strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), action+" AGENT") {
			t.Fatal(action, code, out.String(), diagnostic.String())
		}
		if protocol.ReadOnly("plugin.events."+action) != (action != "subscribe" && action != "close") {
			t.Fatal("incorrect effect classification", action)
		}
	}
}
