package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func treeRequest(root, op string) protocol.Request {
	r, _ := protocol.NewRequest("agent.stop", op, map[string]any{"agent_id": root, "tree": true})
	return r
}

func requireTreeFence(t *testing.T, err error, operation string) {
	t.Helper()
	var wire *protocol.Error
	if !errors.As(err, &wire) || wire.Data.Kind != "tree_stopping" || wire.Data.OperationID != operation {
		t.Fatalf("expected fence %s, got %v", operation, err)
	}
}

func TestTreeStopCapturesScopeAndPersistsAdmissionFence(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, root := controlWork(t, s)
	if _, err := s.ClaimNext(ctx, root.AgentID); err != nil {
		t.Fatal(err)
	}
	child := resultChild(t, s, root.AgentID)
	grandchild := resultChild(t, s, child.AgentID)
	_, unrelated := controlWork(t, s)
	queued := enqueue(t, s, ctx, child.AgentID, "retained input")
	mail := sendMail(t, s, child.AgentID, "steer", "retained mail")
	caller := resultCaller(t, s, root, "existing-shell")
	beforeUnrelated, _ := s.Snapshot(ctx, unrelated.AgentID)
	beforeContext, _, _ := s.ModelContext(ctx, root.AgentID)
	req := treeRequest(root.AgentID, "tree-once")
	accepted, err := s.BeginTreeStop(ctx, req, root.AgentID)
	if err != nil || accepted.AgentID != root.AgentID || accepted.TaskID != "" || accepted.State != "accepted" {
		t.Fatal(accepted, err)
	}
	plan, err := s.TreeStop(ctx, req.OperationID)
	if err != nil || plan.State != "stopping" || len(plan.Members) != 3 || len(plan.Worktrees) != 0 {
		t.Fatal(plan, err)
	}
	for i, a := range []protocol.Accepted{root, child, grandchild} {
		if plan.Members[i] != (TreeStopMember{AgentID: a.AgentID, TaskID: a.TaskID}) {
			t.Fatal("scope mismatch", plan)
		}
		snap, err := s.Snapshot(ctx, a.AgentID)
		if err != nil || !snap.Agent.Paused || snap.Agent.State != "cancelling" {
			t.Fatal(snap, err)
		}
		if w, err := s.ClaimNext(ctx, a.AgentID); err != nil || w != nil {
			t.Fatal("frozen Agent scheduled", w, err)
		}
		if err := s.AppendTaskEvent(ctx, a.AgentID, a.TaskID, "tool.dispatching", struct{}{}); err == nil {
			t.Fatal("new dispatch after freeze")
		}
	}
	afterUnrelated, _ := s.Snapshot(ctx, unrelated.AgentID)
	if !reflect.DeepEqual(beforeUnrelated, afterUnrelated) {
		t.Fatal("tree stop affected unrelated Agent")
	}
	contextAfter, _, _ := s.ModelContext(ctx, root.AgentID)
	if !reflect.DeepEqual(beforeContext, contextAfter) {
		t.Fatal("freeze rewrote model history")
	}
	snap, _ := s.Snapshot(ctx, child.AgentID)
	if snap.Tasks[1].ID != queued.TaskID || snap.Tasks[1].State != "accepted" {
		t.Fatal("queued input was lost", snap)
	}
	if m, err := s.Mail(ctx, mail.MessageID); err != nil || m.State != "accepted" || m.Input != "retained mail" {
		t.Fatal(m, err)
	}
	for _, agent := range []string{root.AgentID, child.AgentID, grandchild.AgentID} {
		target := protocol.Target{AgentID: agent}
		_, err := s.Resume(ctx, resumeRequest(agent, protocol.NewID("resume")), target, 1000)
		requireTreeFence(t, err, req.OperationID)
		p := protocol.Submit{AgentID: agent, Input: "cannot wake"}
		r, _ := protocol.NewRequest("agent.submit", protocol.NewID("submit"), p)
		_, err = s.Submit(ctx, r, p, 1000)
		requireTreeFence(t, err, req.OperationID)
		send := protocol.SendMessage{AgentID: agent, Input: "new mail"}
		r, _ = protocol.NewRequest("agent.send", protocol.NewID("send"), send)
		_, err = s.Send(ctx, r, send, 1000)
		requireTreeFence(t, err, req.OperationID)
		selection, _ := s.Selection(ctx, agent)
		pSpawn := protocol.SpawnAgent{ParentID: agent, Input: "cannot escape"}
		r, _ = protocol.NewRequest("agent.spawn", protocol.NewID("spawn"), pSpawn)
		_, err = s.Spawn(ctx, r, pSpawn, selection.Choice.Cwd, selection.Choice.Revision, 1000, 8)
		requireTreeFence(t, err, req.OperationID)
		r, _ = protocol.NewRequest("agent.select", protocol.NewID("select"), protocol.SelectAgent{AgentID: agent})
		_, err = s.Select(ctx, r, selection, selection.Choice.Revision, false)
		requireTreeFence(t, err, req.OperationID)
	}
	// An already-running command from this tree cannot dispatch new work into
	// a different Agent while the tree stop is in progress.
	for _, method := range []string{"submit", "send"} {
		var err error
		if method == "submit" {
			p := protocol.Submit{AgentID: unrelated.AgentID, Caller: caller, Input: "late"}
			r, _ := protocol.NewRequest("agent.submit", "late-submit", p)
			_, err = s.Submit(ctx, r, p, 1000)
		} else {
			p := protocol.SendMessage{AgentID: unrelated.AgentID, Caller: caller, Input: "late"}
			r, _ := protocol.NewRequest("agent.send", "late-send", p)
			_, err = s.Send(ctx, r, p, 1000)
		}
		requireTreeFence(t, err, req.OperationID)
	}
	// Known tool effects can still settle; pause alone never reports that
	// external processes or workspace preparation have terminated.
	if err := s.RecordToolResult(ctx, root.AgentID, root.TaskID, "request", struct{}{}, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "call", Text: "saved result"}}}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []protocol.Accepted{root, child, grandchild} {
		if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "late final", nil); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.Snapshot(ctx, root.AgentID)
	again, err := s.BeginTreeStop(ctx, req, root.AgentID)
	if err != nil || !reflect.DeepEqual(accepted, again) {
		t.Fatal("retry changed acceptance", again, err)
	}
	after, _ := s.Snapshot(ctx, root.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("retry appended events")
	}
	observed, err := s.TreeStop(ctx, req.OperationID)
	if err != nil || !reflect.DeepEqual(plan, observed) || observed.State != "stopping" {
		t.Fatal("task exit falsely completed tree stop", observed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.TreeStop(ctx, req.OperationID)
	if err != nil || !reflect.DeepEqual(plan, saved) {
		t.Fatal(saved, err)
	}
	_, err = reopened.Resume(ctx, resumeRequest(root.AgentID, "resume-after-reopen"), protocol.Target{AgentID: root.AgentID}, 1000)
	requireTreeFence(t, err, req.OperationID)
	_, err = reopened.BeginTreeStop(ctx, treeRequest(child.AgentID, req.OperationID), child.AgentID)
	var wire *protocol.Error
	if !errors.As(err, &wire) || wire.Data.Kind != "operation_conflict" {
		t.Fatal("request ID changed scope", err)
	}
}

func TestTreeStopCapturesAllPendingWorktreeStates(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	root := mailAgent(t, s)
	other := mailAgent(t, s)
	var want []string
	var preparedReq protocol.Request
	var acceptedReq protocol.Request
	var acceptedChild protocol.Accepted
	for _, state := range []string{"prepared", "creating", "unknown", "ready", "accepted", "unrelated"} {
		parent := root.AgentID
		if state == "unrelated" {
			parent = other.AgentID
		}
		p := protocol.SpawnAgent{ParentID: parent, Input: "work", Workspace: "worktree", WorktreePath: "/fixture/" + state}
		req, _ := protocol.NewRequest("agent.spawn", "workspace-"+state, p)
		plan := protocol.Worktree{Source: "/fixture", Repository: "/fixture/.git", Path: p.WorktreePath, Owner: "owner-" + state, Commit: "fixture-commit"}
		if _, err := s.PrepareWorktree(ctx, req, p, plan, 0, 1000, 8); err != nil {
			t.Fatal(err)
		}
		if state == "prepared" {
			preparedReq = req
		}
		if state != "prepared" && state != "unrelated" {
			if _, err := s.TransitionWorktree(ctx, req, "prepared", "creating", nil); err != nil {
				t.Fatal(err)
			}
		}
		if state == "unknown" || state == "ready" || state == "accepted" {
			next := state
			if state == "accepted" {
				next = "ready"
			}
			if _, err := s.TransitionWorktree(ctx, req, "creating", next, nil); err != nil {
				t.Fatal(err)
			}
		}
		if state == "accepted" {
			var err error
			acceptedChild, err = s.Spawn(ctx, req, p, plan.Path, 0, 1000, 8)
			if err != nil {
				t.Fatal(err)
			}
			acceptedReq = req
		} else if state != "unrelated" {
			want = append(want, req.OperationID)
		}
	}
	req := treeRequest(root.AgentID, "stop-workspaces")
	if _, err := s.BeginTreeStop(ctx, req, root.AgentID); err != nil {
		t.Fatal(err)
	}
	plan, err := s.TreeStop(ctx, req.OperationID)
	if err != nil || !slices.Equal(plan.Worktrees, want) || len(plan.Members) != 2 {
		t.Fatal(plan, want, err)
	}
	for _, id := range want {
		r, err := s.WorktreeOperation(ctx, id)
		if err != nil || r.State == "cancelled" {
			t.Fatal("freeze falsely reports Git exit", r, err)
		}
	}
	if _, err := s.TransitionWorktree(ctx, preparedReq, "prepared", "creating", nil); err == nil {
		t.Fatal("new Git dispatch after freeze")
	} else {
		requireTreeFence(t, err, req.OperationID)
	}
	var pending protocol.SpawnAgent
	json.Unmarshal(preparedReq.Params, &pending)
	pending.WorktreePath = "/fixture/new"
	newReq, _ := protocol.NewRequest("agent.spawn", "new-workspace", pending)
	_, err = s.PrepareWorktree(ctx, newReq, pending, protocol.Worktree{Path: pending.WorktreePath, Repository: "/fixture/.git", Owner: "new", Commit: "commit"}, 0, 1000, 8)
	requireTreeFence(t, err, req.OperationID)
	// A verified completion may be recorded, but cannot publish a new child.
	creatingReq, _ := protocol.NewRequest("agent.spawn", "workspace-creating", protocol.SpawnAgent{ParentID: root.AgentID, Input: "work", Workspace: "worktree", WorktreePath: "/fixture/creating"})
	if _, err := s.TransitionWorktree(ctx, creatingReq, "creating", "ready", nil); err != nil {
		t.Fatal(err)
	}
	var creating protocol.SpawnAgent
	json.Unmarshal(creatingReq.Params, &creating)
	_, err = s.Spawn(ctx, creatingReq, creating, creating.WorktreePath, 0, 1000, 8)
	requireTreeFence(t, err, req.OperationID)
	var acceptedParams protocol.SpawnAgent
	json.Unmarshal(acceptedReq.Params, &acceptedParams)
	again, err := s.Spawn(ctx, acceptedReq, acceptedParams, "/no-longer-used", -1, 0, 0)
	if err != nil || !reflect.DeepEqual(acceptedChild, again) {
		t.Fatal("freeze broke original accepted retry", again, err)
	}
}

func TestTreeStopOverlapAndFailureRollBackWholeScope(t *testing.T) {
	for _, variant := range []string{"overlap", "pause-event", "scope-event", "receipt", "archived"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			_, root := controlWork(t, s)
			child := resultChild(t, s, root.AgentID)
			switch variant {
			case "overlap":
				if _, err := s.BeginTreeStop(ctx, treeRequest(child.AgentID, "child-stop"), child.AgentID); err != nil {
					t.Fatal(err)
				}
			case "pause-event":
				if _, err := s.db.Exec(`CREATE TRIGGER fail_tree BEFORE INSERT ON events WHEN NEW.kind='agent.tree_stopping' AND NEW.agent_id='` + child.AgentID + `' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			case "scope-event":
				if _, err := s.db.Exec(`CREATE TRIGGER fail_tree BEFORE INSERT ON events WHEN NEW.kind='tree_stop.started' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				if _, err := s.db.Exec(`CREATE TRIGGER fail_tree BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			case "archived":
				if _, err := s.db.Exec("UPDATE agents SET state='archived' WHERE id=?", child.AgentID); err != nil {
					t.Fatal(err)
				}
			}
			rootBefore, _ := s.Snapshot(ctx, root.AgentID)
			childBefore, _ := s.Snapshot(ctx, child.AgentID)
			req := treeRequest(root.AgentID, "failed-tree")
			accepted, err := s.BeginTreeStop(ctx, req, root.AgentID)
			if err == nil || accepted.AgentID != "" {
				t.Fatal("partial stop accepted", accepted, err)
			}
			if variant == "overlap" {
				requireTreeFence(t, err, "child-stop")
			}
			rootAfter, _ := s.Snapshot(ctx, root.AgentID)
			childAfter, _ := s.Snapshot(ctx, child.AgentID)
			if !reflect.DeepEqual(rootBefore, rootAfter) || !reflect.DeepEqual(childBefore, childAfter) {
				t.Fatal("failed stop left a partial pause")
			}
			if _, err := s.Operation(ctx, req.OperationID); err == nil {
				t.Fatal("failed tree retained acceptance")
			}
			if _, err := s.TreeStop(ctx, req.OperationID); err == nil {
				t.Fatal("failed tree retained plan")
			}
		})
	}
}

func TestTreeStopSpawnRaceIncludesOrRejectsChild(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	for range 30 {
		root := mailAgent(t, s)
		selection, _ := s.Selection(ctx, root.AgentID)
		p := protocol.SpawnAgent{ParentID: root.AgentID, Input: "concurrent work", Context: "none"}
		spawnReq, _ := protocol.NewRequest("agent.spawn", protocol.NewID("spawn"), p)
		stopReq := treeRequest(root.AgentID, protocol.NewID("stop"))
		var child protocol.Accepted
		var spawnErr, stopErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			child, spawnErr = s.Spawn(ctx, spawnReq, p, selection.Choice.Cwd, selection.Choice.Revision, 1000, 8)
		}()
		go func() {
			defer wg.Done()
			_, stopErr = s.BeginTreeStop(ctx, stopReq, root.AgentID)
		}()
		wg.Wait()
		if stopErr != nil {
			t.Fatal(stopErr)
		}
		plan, err := s.TreeStop(ctx, stopReq.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if spawnErr != nil {
			requireTreeFence(t, spawnErr, stopReq.OperationID)
			if len(plan.Members) != 1 {
				t.Fatal("failed spawn retained child", plan)
			}
		} else {
			if len(plan.Members) != 2 || plan.Members[1].AgentID != child.AgentID {
				t.Fatal("child escaped snapshot", child, plan)
			}
			snap, _ := s.Snapshot(ctx, child.AgentID)
			if !snap.Agent.Paused || snap.Agent.State != "paused" {
				t.Fatal("child escaped freeze", snap)
			}
		}
	}
}

func TestTreeStopRequiresExplicitMatchingScopeAndKeepsUnknown(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, root := controlWork(t, s)
	s.ClaimNext(ctx, root.AgentID)
	if err := s.FinishTask(ctx, root.AgentID, root.TaskID, "unknown", "", protocol.E("unknown", "fixture")); err != nil {
		t.Fatal(err)
	}
	for _, req := range []protocol.Request{stopRequest(root.AgentID, "ordinary"), treeRequest("missing", "mismatch")} {
		if _, err := s.BeginTreeStop(ctx, req, root.AgentID); err == nil {
			t.Fatal("implicit/mismatched tree scope accepted")
		}
	}
	if _, err := s.BeginTreeStop(ctx, treeRequest(root.AgentID, "unknown-tree"), root.AgentID); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx, root.AgentID)
	if snap.Agent.State != "unknown" || !snap.Agent.Paused || snap.Tasks[0].State != "cancelling" {
		t.Fatal("unknown effect was declared stopped", snap)
	}
	if err := s.FinishTask(ctx, root.AgentID, root.TaskID, "cancelled", "", nil); err == nil {
		t.Fatal("unknown effect skipped verification")
	}
}

func TestTreeStopGateReleasesOnlyOnOriginalRootCompletion(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	root := mailAgent(t, s)
	child := resultChild(t, s, root.AgentID)
	req := treeRequest(root.AgentID, "original-tree")
	if _, err := s.BeginTreeStop(ctx, req, root.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, child.AgentID, child.TaskID, "completed", "", nil); err != nil {
		t.Fatal(err)
	}
	// Fixture completion events exercise the projection contract. Only the
	// future runtime orchestrator may publish the real root completion after
	// verifying all external effects; this is not process-exit evidence.
	for _, item := range [][2]string{{child.AgentID, req.OperationID}, {root.AgentID, "another-tree"}} {
		if _, err := s.Append(ctx, item[0], "", "tree_stop.completed", map[string]string{"operation_id": item[1]}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Resume(ctx, resumeRequest(child.AgentID, "early"), protocol.Target{AgentID: child.AgentID}, 1000)
	requireTreeFence(t, err, req.OperationID)
	if _, err := s.Append(ctx, root.AgentID, "", "tree_stop.completed", map[string]string{"operation_id": req.OperationID}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root.AgentID, child.AgentID} {
		snap, _ := s.Snapshot(ctx, id)
		if !snap.Agent.Paused {
			t.Fatal("tree completion implicitly resumed Agent")
		}
		if _, err := s.Resume(ctx, resumeRequest(id, protocol.NewID("resume")), protocol.Target{AgentID: id}, 1000); err != nil {
			t.Fatal(err)
		}
	}
	record, err := s.TreeStop(ctx, req.OperationID)
	if err != nil || record.State != "stopped" {
		t.Fatal(record, err)
	}
	before, _ := s.Snapshot(ctx, root.AgentID)
	if _, err := s.BeginTreeStop(ctx, req, root.AgentID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Snapshot(ctx, root.AgentID)
	if !reflect.DeepEqual(before, after) || after.Agent.Paused {
		t.Fatal("retry of finished tree stop paused newer work")
	}
}

func TestTreeStopWorktreePreparationAndAcceptanceRaces(t *testing.T) {
	for _, phase := range []string{"prepare", "accept"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			for i := range 20 {
				root := mailAgent(t, s)
				p := protocol.SpawnAgent{ParentID: root.AgentID, Input: "work", Context: "none", Workspace: "worktree", WorktreePath: fmt.Sprintf("/fixture/%s/%d", phase, i)}
				req, _ := protocol.NewRequest("agent.spawn", protocol.NewID("workspace"), p)
				workspace := protocol.Worktree{Source: "/fixture", Repository: "/fixture/.git", Path: p.WorktreePath, Owner: "owner", Commit: "commit"}
				if phase == "accept" {
					if _, err := s.PrepareWorktree(ctx, req, p, workspace, 0, 1000, 8); err != nil {
						t.Fatal(err)
					}
					for _, change := range [][2]string{{"prepared", "creating"}, {"creating", "ready"}} {
						if _, err := s.TransitionWorktree(ctx, req, change[0], change[1], nil); err != nil {
							t.Fatal(err)
						}
					}
				}
				stopReq := treeRequest(root.AgentID, protocol.NewID("stop"))
				var child protocol.Accepted
				var workspaceErr, stopErr error
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					if phase == "prepare" {
						_, workspaceErr = s.PrepareWorktree(ctx, req, p, workspace, 0, 1000, 8)
					} else {
						child, workspaceErr = s.Spawn(ctx, req, p, workspace.Path, 0, 1000, 8)
					}
				}()
				go func() {
					defer wg.Done()
					_, stopErr = s.BeginTreeStop(ctx, stopReq, root.AgentID)
				}()
				wg.Wait()
				if stopErr != nil {
					t.Fatal(stopErr)
				}
				if workspaceErr != nil {
					requireTreeFence(t, workspaceErr, stopReq.OperationID)
				}
				plan, err := s.TreeStop(ctx, stopReq.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				capturedWorkspace := slices.Contains(plan.Worktrees, req.OperationID)
				switch phase {
				case "prepare":
					if capturedWorkspace != (workspaceErr == nil) || len(plan.Members) != 1 {
						t.Fatal("preparation escaped or phantom intent captured", plan, workspaceErr)
					}
				case "accept":
					if workspaceErr == nil {
						if capturedWorkspace || len(plan.Members) != 2 || plan.Members[1].AgentID != child.AgentID {
							t.Fatal("accepted checkout must be covered by its child", plan, child)
						}
					} else if !capturedWorkspace || len(plan.Members) != 1 {
						t.Fatal("unaccepted checkout was lost from stop scope", plan)
					}
				}
			}
		})
	}
}

func TestTreeStopSubmitAndResumeRacesCannotReleaseFence(t *testing.T) {
	ctx := context.Background()
	for _, method := range []string{"submit", "resume"} {
		t.Run(method, func(t *testing.T) {
			s, _ := newStore(t)
			for range 20 {
				root := mailAgent(t, s)
				mailControl(t, s, "stop", root.AgentID)
				stopReq := treeRequest(root.AgentID, protocol.NewID("tree"))
				var admissionErr, stopErr error
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					if method == "submit" {
						p := protocol.Submit{AgentID: root.AgentID, Input: "new explicit input"}
						req, _ := protocol.NewRequest("agent.submit", protocol.NewID("submit"), p)
						_, admissionErr = s.Submit(ctx, req, p, 1000)
					} else {
						_, admissionErr = s.Resume(ctx, resumeRequest(root.AgentID, protocol.NewID("resume")), protocol.Target{AgentID: root.AgentID}, 1000)
					}
				}()
				go func() {
					defer wg.Done()
					_, stopErr = s.BeginTreeStop(ctx, stopReq, root.AgentID)
				}()
				wg.Wait()
				if stopErr != nil {
					t.Fatal(stopErr)
				}
				if admissionErr != nil {
					requireTreeFence(t, admissionErr, stopReq.OperationID)
				}
				snap, err := s.Snapshot(ctx, root.AgentID)
				if err != nil || !snap.Agent.Paused || snap.Agent.State != "paused" {
					t.Fatal("concurrent admission released fence", snap, err)
				}
				if work, err := s.ClaimNext(ctx, root.AgentID); err != nil || work != nil {
					t.Fatal("concurrent admission scheduled frozen work", work, err)
				}
			}
		})
	}
}

func TestTreeStopBoundRejectsWithoutPartialPause(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	root := mailAgent(t, s)
	// Build historical identities directly; the scope bound is independent of
	// configured active-task capacity.
	if err := s.Transaction(ctx, func(tx *sql.Tx) error {
		for i := range maxTreeStopMembers {
			if _, err := tx.ExecContext(ctx, "INSERT INTO agents(id,parent_id,cwd,state) VALUES(?,?,?,'idle')", fmt.Sprint("child-", i), root.AgentID, "/fixture"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.BeginTreeStop(ctx, treeRequest(root.AgentID, "oversized"), root.AgentID)
	var wire *protocol.Error
	if !errors.As(err, &wire) || wire.Data.Kind != "limit_exceeded" {
		t.Fatal(err)
	}
	var paused int
	if err := s.db.QueryRow("SELECT count(*) FROM agents WHERE paused=1").Scan(&paused); err != nil || paused != 0 {
		t.Fatal("partial oversized stop", paused, err)
	}
}

func TestTreeStopSchemaElevenMigrationPreservesFacts(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, root := controlWork(t, s)
	before, _ := s.Snapshot(ctx, root.AgentID)
	if _, err := s.db.Exec(dropPluginSubscriptionSchema + "DROP TABLE plugin_progress; DROP TABLE plugin_calls; DROP TABLE mcp_calls; DROP TABLE resource_updates; DROP INDEX tree_stop_events; PRAGMA user_version=11;"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, _ := reopened.Snapshot(ctx, root.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed existing facts")
	}
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
	if _, err := reopened.BeginTreeStop(ctx, treeRequest(root.AgentID, "after-upgrade"), root.AgentID); err != nil {
		t.Fatal(err)
	}
}
