package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"zende.local/harness/internal/protocol"
)

func prepareWorkspaceFixture(t *testing.T, s *Store) (protocol.Request, protocol.SpawnAgent, protocol.Worktree) {
	t.Helper()
	_, parent := create(t, s)
	p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "isolated task", Workspace: "worktree", WorktreePath: "../child"}
	req, _ := protocol.NewRequest("agent.spawn", "workspace-once", p)
	plan := protocol.Worktree{Source: "/source", Repository: "/source/.git", Path: filepath.Join(t.TempDir(), "child"), Commit: "commit-fixture", Owner: "owner-fixture"}
	return req, p, plan
}

func TestWorktreeIntentReceiptRetryAndGlobalOperationIdentity(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	req, p, plan := prepareWorkspaceFixture(t, s)
	r, err := s.PrepareWorktree(ctx, req, p, plan, 0, 8, 4)
	if err != nil || r.Operation.State != "prepared" {
		t.Fatal(r, err)
	}
	changedCaller := p
	changedCaller.Caller = &protocol.Caller{AgentID: "later", TaskID: "later", OperationID: "later"}
	retry, _ := protocol.NewRequest("agent.spawn", req.OperationID, changedCaller)
	again, err := s.PrepareWorktree(ctx, retry, changedCaller, protocol.Worktree{Path: "/other", Repository: "/other/.git", Owner: "other", Commit: "other"}, -1, 0, 0)
	if err != nil || !reflect.DeepEqual(r, again) {
		t.Fatal("preparation retry changed first identity", again, err)
	}
	conflicting, _ := protocol.NewRequest("agent.create", req.OperationID, protocol.CreateAgent{Cwd: "/"})
	if _, _, err := s.AcceptedRequest(ctx, conflicting); err == nil {
		t.Fatal("prepared ID accepted a different method")
	}
	if _, err := s.Create(ctx, conflicting, protocol.CreateAgent{Cwd: "/"}, 8); err == nil {
		t.Fatal("acceptance bypassed reservation")
	}
	if _, err := s.Spawn(ctx, req, p, plan.Path, 0, 8, 4); err == nil {
		t.Fatal("unready worktree became an Agent")
	}
	for _, transition := range [][2]string{{"prepared", "creating"}, {"creating", "unknown"}, {"unknown", "ready"}} {
		if _, err := s.TransitionWorktree(ctx, req, transition[0], transition[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	accepted, err := s.Spawn(ctx, retry, changedCaller, plan.Path, 0, 8, 4)
	if err != nil || !reflect.DeepEqual(accepted.Workspace, &plan) || accepted.Spawn.Caller != nil {
		t.Fatal(accepted, err)
	}
	snapshot, _ := s.Snapshot(ctx, accepted.AgentID)
	if snapshot.Agent.Cwd != plan.Path || snapshot.Agent.ParentID != p.ParentID {
		t.Fatal(snapshot)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	old, found, err := reopened.AcceptedRequest(ctx, retry)
	if err != nil || !found || !reflect.DeepEqual(old, accepted) {
		t.Fatal(old, found, err)
	}
	saved, err := reopened.WorktreeRecord(ctx, retry)
	if err != nil || saved.Operation.State != "ready" || saved.Params.Caller != nil {
		t.Fatal(saved, err)
	}
	for _, statement := range []string{
		"UPDATE workspace_operations SET target='/changed'",
		"UPDATE workspace_operations SET request='{}'",
		"UPDATE workspace_operations SET plan='{}'",
		"DELETE FROM workspace_operations",
	} {
		if _, err := reopened.db.Exec(statement); err == nil {
			t.Fatal("immutable intent changed", statement)
		}
	}
}

func TestWorktreePreparationPreflightAndTransactionalFailures(t *testing.T) {
	for _, variant := range []string{"capacity", "depth", "context", "caller", "revision", "prepare-event", "transition-event", "accept-receipt", "reserved-target"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			req, p, plan := prepareWorkspaceFixture(t, s)
			maxAgents, maxDepth, revision := 8, 4, int64(0)
			switch variant {
			case "capacity":
				maxAgents = 1
			case "depth":
				maxDepth = 0
			case "context":
				p.Context = "last:0"
			case "caller":
				p.Caller = &protocol.Caller{AgentID: p.ParentID, TaskID: "missing", OperationID: "missing"}
			case "revision":
				revision = 99
			case "prepare-event":
				_, err := s.db.Exec("CREATE TRIGGER fail_prepare BEFORE INSERT ON events WHEN NEW.kind='workspace.prepared' BEGIN SELECT RAISE(ABORT,'fixture'); END")
				if err != nil {
					t.Fatal(err)
				}
			}
			req.Params, _ = json.Marshal(p)
			r, err := s.PrepareWorktree(ctx, req, p, plan, revision, maxAgents, maxDepth)
			if variant != "transition-event" && variant != "accept-receipt" && variant != "reserved-target" {
				if err == nil || r != nil {
					t.Fatal(r, err)
				}
				if saved, _ := s.WorktreeRecord(ctx, req); saved != nil {
					t.Fatal("partial preparation", saved)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if variant == "reserved-target" {
				other := req
				other.OperationID = "other"
				if _, err := s.PrepareWorktree(ctx, other, p, plan, 0, 8, 4); err == nil {
					t.Fatal("two operations own one path")
				}
				return
			}
			if variant == "transition-event" {
				if _, err := s.db.Exec("CREATE TRIGGER fail_transition BEFORE INSERT ON events WHEN NEW.kind='workspace.creating' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.TransitionWorktree(ctx, req, "prepared", "creating", nil); err == nil {
					t.Fatal("injected transition succeeded")
				}
				saved, _ := s.WorktreeRecord(ctx, req)
				if saved.Operation.State != "prepared" {
					t.Fatal("partial transition", saved)
				}
				return
			}
			if _, err := s.TransitionWorktree(ctx, req, "prepared", "creating", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.TransitionWorktree(ctx, req, "creating", "ready", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("CREATE TRIGGER fail_accept BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
				t.Fatal(err)
			}
			if a, err := s.Spawn(ctx, req, p, plan.Path, 0, 8, 4); err == nil || a.AgentID != "" {
				t.Fatal(a, err)
			}
			agents, _ := s.ListAgents(ctx)
			if len(agents) != 1 {
				t.Fatal("partial child", agents)
			}
			saved, _ := s.WorktreeRecord(ctx, req)
			if saved.Operation.State != "ready" {
				t.Fatal("lost completed checkout")
			}
			if _, err := s.db.Exec("DROP TRIGGER fail_accept"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Spawn(ctx, req, p, plan.Path, 0, 8, 4); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorktreeSchemaEightMigrationPreservesContextView(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, parent := create(t, s)
	_, _ = s.ClaimNext(ctx, parent.AgentID)
	before, _ := s.ContextSnapshot(ctx, parent.AgentID)
	if _, err := s.PublishContextView(ctx, parent.AgentID, ContextView{SourceCursor: before.Cursor, Through: before.Cursor, Summary: "kept"}); err != nil {
		t.Fatal(err)
	}
	before, _ = s.ContextSnapshot(ctx, parent.AgentID)
	if _, err := s.db.Exec("DROP TABLE mailbox; DROP TABLE workspace_operations; PRAGMA user_version=8"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.ContextSnapshot(ctx, parent.AgentID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal(after, err)
	}
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
}
