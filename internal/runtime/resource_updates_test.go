package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

func TestResourceFailedInstallCanBeInspectedAndRepaired(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := New(s, resource.Effective{Config: resource.Defaults(dir)})
	create := protocol.CreateAgent{Cwd: dir}
	req, _ := protocol.NewRequest("agent.create", "agent", create)
	a, err := s.Create(ctx, req, create, 8)
	if err != nil {
		t.Fatal(err)
	}
	input := protocol.RegisterResource{AgentID: a.AgentID, Kind: "skill", Path: "incomplete"}
	req, _ = protocol.NewRequest("resource.register", "install", input)
	if _, err := c.Handle(ctx, req); err == nil {
		t.Fatal("missing installation accepted")
	} else {
		var known *protocol.Error
		if !errors.As(err, &known) || known.Data.Kind != "invalid_resource" || known.Data.ResourceID == "" || known.Data.OperationID != req.OperationID {
			t.Fatal("missing lookup identity", err)
		}
		status, err := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: known.Data.ResourceID})
		if err != nil || status.State != "failed" || status.Version != "" || status.Update.State != "failed" {
			t.Fatal(status, err)
		}
	}
	identity, _ := resource.Identity(input.Kind, filepath.Join(dir, input.Path))
	if err := os.Mkdir(filepath.Dir(identity.Source), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(identity.Source, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("---\nname: partial")
	refresh, _ := protocol.NewRequest("resource.refresh", "refresh", protocol.ResourceTarget{AgentID: a.AgentID, ID: identity.ID})
	if _, err := c.Handle(ctx, refresh); err == nil {
		t.Fatal("partial skill published")
	}
	page, _ := s.Resources(ctx, protocol.ResourceList{AgentID: a.AgentID})
	if len(page.Items) != 0 {
		t.Fatal("failed source exposed as usable", page)
	}
	write("---\nname: repaired\ndescription: Complete installation\n---\nUse this skill.\n")
	got, err := c.Handle(ctx, refresh)
	if err != nil {
		t.Fatal(err)
	}
	accepted := got.(protocol.Accepted)
	if accepted.Resource == nil || accepted.Resource.ID != identity.ID {
		t.Fatal(got)
	}
	status, err := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: identity.ID})
	if err != nil || status.State != "ready" || status.Update.State != "ready" {
		t.Fatal(status, err)
	}
	write("broken again")
	before, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	replay, err := c.Handle(ctx, refresh)
	after, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if err != nil || !reflect.DeepEqual(replay, accepted) || !reflect.DeepEqual(before, after) {
		t.Fatal("acceptance retry reread files or changed history", replay, err)
	}
}

func TestResourceUpdateInterruptedByCoreReplacement(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	params := protocol.CreateAgent{Cwd: dir}
	req, _ := protocol.NewRequest("agent.create", "create", params)
	a, err := s.Create(ctx, req, params, 8)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := resource.Identity("skill", filepath.Join(dir, "install"))
	req, _ = protocol.NewRequest("resource.register", "pending", protocol.RegisterResource{AgentID: a.AgentID, Kind: "skill", Path: identity.Source})
	if _, _, err := s.BeginResourceUpdate(ctx, req, a.AgentID, identity, "terminated-core"); err != nil {
		t.Fatal(err)
	}
	c := New(s, resource.Effective{Config: resource.Defaults(dir)})
	running, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(running) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: identity.ID})
		if err != nil {
			t.Fatal(err)
		}
		if status.Update != nil && status.Update.State == "failed" {
			if status.State != "failed" || status.Update.Error.Data.Kind != "interrupted" || status.Version != "" {
				t.Fatal(status)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("new core left old preparation pending")
}
