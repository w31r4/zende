package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func beginResource(t *testing.T, s *Store, agent, op, owner, source string) (protocol.Request, ResourcePreparation) {
	t.Helper()
	identity, err := resource.Identity("skill", source)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := protocol.NewRequest("resource.register", op, protocol.RegisterResource{AgentID: agent, Kind: "skill", Path: source})
	p, accepted, err := s.BeginResourceUpdate(context.Background(), req, agent, identity, owner)
	if err != nil || accepted != nil {
		t.Fatal(p, accepted, err)
	}
	return req, p
}

func TestResourceUpdateFailureAndReadyVersionAreIndependent(t *testing.T) {
	ctx := context.Background()
	s, db := newStore(t)
	_, a := create(t, s)
	req, first := beginResource(t, s, a.AgentID, "install", "core-1", "/skills/review")
	target := protocol.ResourceTarget{AgentID: a.AgentID, ID: first.Identity.ID}
	status, err := s.ResourceStatus(ctx, target)
	if err != nil || status.State != "preparing" || status.Version != "" || status.Generation != 0 || status.Update.AttemptID != first.Update.AttemptID {
		t.Fatal(status, err)
	}
	page, err := s.Resources(ctx, protocol.ResourceList{AgentID: a.AgentID})
	if err != nil || len(page.Items) != 0 || page.Generation != 0 {
		t.Fatal("unready source entered discovery", page, err)
	}
	if _, err := s.ReadResource(ctx, protocol.ResourceRead{AgentID: a.AgentID, ID: target.ID}); err == nil {
		t.Fatal("unready source became readable")
	}
	if err := s.FailResourceUpdate(ctx, a.AgentID, first, "invalid_resource"); err != nil {
		t.Fatal(err)
	}
	status, err = s.ResourceStatus(ctx, target)
	if err != nil || status.State != "failed" || status.Update.Error.Data.Kind != "invalid_resource" || status.Update.Error.Data.ResourceID != target.ID {
		t.Fatal(status, err)
	}
	if _, found, err := s.AcceptedRequest(ctx, req); err != nil || found {
		t.Fatal("failed preparation accepted", found, err)
	}
	// Failed installation is queryable after reopening and can reuse its request.
	s.Close()
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.ResourceStatus(ctx, target)
	if err != nil || !reflect.DeepEqual(status, got) {
		t.Fatal(got, err)
	}
	req, retry := beginResource(t, s, a.AgentID, "install", "core-2", "/skills/review")
	pkg := savedResource(t, retry.Identity.Source, "complete v1")
	accepted, err := s.PublishResourceUpdate(ctx, req, a.AgentID, pkg, retry)
	if err != nil || accepted.Resource.Version != pkg.Info.Version {
		t.Fatal(accepted, err)
	}
	ready, err := s.ResourceStatus(ctx, target)
	if err != nil || ready.State != "ready" || ready.Update.State != "ready" || ready.Update.Error != nil {
		t.Fatal(ready, err)
	}
	_, bad := beginResource(t, s, a.AgentID, "refresh-bad", "core-2", retry.Identity.Source)
	if err := s.FailResourceUpdate(ctx, a.AgentID, bad, "invalid_resource"); err != nil {
		t.Fatal(err)
	}
	status, err = s.ResourceStatus(ctx, target)
	if err != nil || status.State != "ready" || status.Update.State != "failed" || status.Version != ready.Version || status.Generation != ready.Generation {
		t.Fatal("failed refresh lost ready version", status, err)
	}
	snapshot, err := s.ContextSnapshot(ctx, a.AgentID)
	if err != nil || len(snapshot.Resources.Catalog.Entries) != 1 || snapshot.Resources.Catalog.Entries[0].Version != ready.Version {
		t.Fatal(snapshot, err)
	}
	// Acceptance retry does not re-prepare or hide the most recent failure.
	_, replay, err := s.BeginResourceUpdate(ctx, req, a.AgentID, retry.Identity, "core-2")
	if err != nil || replay == nil || !reflect.DeepEqual(*replay, accepted) {
		t.Fatal(replay, err)
	}
	after, err := s.ResourceStatus(ctx, target)
	if err != nil || !reflect.DeepEqual(after, status) {
		t.Fatal("acceptance retry changed update", after, err)
	}
	// Re-validating identical bytes clears the failure without a new catalog.
	r, same := beginResource(t, s, a.AgentID, "refresh-same", "core-2", retry.Identity.Source)
	if _, err := s.PublishResourceUpdate(ctx, r, a.AgentID, pkg, same); err != nil {
		t.Fatal(err)
	}
	after, err = s.ResourceStatus(ctx, target)
	if err != nil || after.Update.State != "ready" || after.Generation != ready.Generation {
		t.Fatal(after, err)
	}
}

func TestResourceUpdateFencesSlowAndForeignPreparations(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, a := create(t, s)
	r1, slow := beginResource(t, s, a.AgentID, "old", "core", "/skills/review")
	r2, latest := beginResource(t, s, a.AgentID, "new", "core", "/skills/review")
	pkg := savedResource(t, latest.Identity.Source, "body")
	if _, err := s.PublishResourceUpdate(ctx, r1, a.AgentID, pkg, slow); err == nil {
		t.Fatal("superseded preparation published")
	}
	if err := s.FailResourceUpdate(ctx, a.AgentID, slow, "invalid_resource"); err != nil {
		t.Fatal(err)
	}
	status, err := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: pkg.Info.ID})
	if err != nil || status.Update.AttemptID != latest.Update.AttemptID || status.State != "preparing" {
		t.Fatal(status, err)
	}
	for _, altered := range []ResourcePreparation{func() ResourcePreparation { p := latest; p.BaseGeneration++; return p }(), func() ResourcePreparation { p := latest; p.Owner = "other"; return p }()} {
		if _, err := s.PublishResourceUpdate(ctx, r2, a.AgentID, pkg, altered); err == nil {
			t.Fatal("altered preparation published")
		}
	}
	if _, err := s.PublishResourceUpdate(ctx, r1, a.AgentID, pkg, latest); err == nil {
		t.Fatal("foreign request published preparation")
	}
	if _, err := s.PublishResourceUpdate(ctx, r2, a.AgentID, pkg, latest); err != nil {
		t.Fatal(err)
	}
	if err := s.FailResourceUpdate(ctx, a.AgentID, latest, "internal"); err != nil {
		t.Fatal(err)
	}
	status, _ = s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: pkg.Info.ID})
	if status.Update.State != "ready" {
		t.Fatal("late failure changed committed success", status)
	}
}

func TestResourceUpdateAtomicPublicationAndRestart(t *testing.T) {
	for _, failure := range []string{"update", "acceptance"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			s, db := newStore(t)
			_, a := create(t, s)
			original := savedResource(t, "/skills/review/SKILL.md", "v1")
			publication(t, s, a.AgentID, "original", original, 0)
			req, pending := beginResource(t, s, a.AgentID, "change", "dead-core", original.Info.Source)
			pkg := savedResource(t, original.Info.Source, "v2")
			trigger := "BEFORE UPDATE ON resource_updates"
			if failure == "acceptance" {
				trigger = "BEFORE INSERT ON operations"
			}
			if _, err := s.db.Exec("CREATE TRIGGER fail_commit " + trigger + " BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
			if _, err := s.PublishResourceUpdate(ctx, req, a.AgentID, pkg, pending); err == nil {
				t.Fatal("expected failure")
			}
			after, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
			if !reflect.DeepEqual(before, after) {
				t.Fatal("partial publication escaped rollback")
			}
			if _, found, _ := s.AcceptedRequest(ctx, req); found {
				t.Fatal("partial publication accepted")
			}
			if _, err := s.ReadResource(ctx, protocol.ResourceRead{AgentID: a.AgentID, ID: pkg.Info.ID, Version: pkg.Info.Version}); err == nil {
				t.Fatal("partial version readable")
			}
			if _, err := s.db.Exec("DROP TRIGGER fail_commit"); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err := Open(db)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, live := beginResource(t, s, a.AgentID, "live", "new-core", "/skills/another")
			if err := s.InterruptResourceUpdates(ctx, "new-core"); err != nil {
				t.Fatal(err)
			}
			status, err := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: pkg.Info.ID})
			if err != nil || status.State != "ready" || status.Version != original.Info.Version || status.Update.State != "failed" || status.Update.Error.Data.Kind != "interrupted" {
				t.Fatal(status, err)
			}
			stillLive, err := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: live.Identity.ID})
			if err != nil || stillLive.State != "preparing" {
				t.Fatal("interrupted current core", stillLive, err)
			}
			before, _ = s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
			if err := s.InterruptResourceUpdates(ctx, "new-core"); err != nil {
				t.Fatal(err)
			}
			after, _ = s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
			if !reflect.DeepEqual(before, after) {
				t.Fatal("duplicate interrupted event")
			}
		})
	}
}

func TestResourceUpdateLimitsIsolationAndMigration(t *testing.T) {
	ctx := context.Background()
	s, db := newStore(t)
	_, a := create(t, s)
	pkg := savedResource(t, "/skills/review/SKILL.md", "v1")
	req, accepted := publication(t, s, a.AgentID, "initial", pkg, 0)
	before, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if _, err := s.db.Exec("DROP TABLE mcp_calls; DROP TABLE resource_updates; PRAGMA user_version=12"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed history")
	}
	if got, found, err := s.AcceptedRequest(ctx, req); err != nil || !found || !reflect.DeepEqual(got, accepted) {
		t.Fatal(got, found, err)
	}
	for i := range protocol.MaxAgentResources - 1 {
		_, attempt := beginResource(t, s, a.AgentID, fmt.Sprint("invalid-", i), "core", fmt.Sprintf("/skills/bad-%d", i))
		if err := s.FailResourceUpdate(ctx, a.AgentID, attempt, "invalid_resource"); err != nil {
			t.Fatal(err)
		}
	}
	identity, _ := resource.Identity("skill", "/skills/excess")
	r, _ := protocol.NewRequest("resource.register", "excess", protocol.RegisterResource{AgentID: a.AgentID, Kind: "skill", Path: identity.Source})
	if _, _, err := s.BeginResourceUpdate(ctx, r, a.AgentID, identity, "core"); err == nil || !strings.Contains(err.Error(), "resource_limit") {
		t.Fatal("pending source limit", err)
	}
	// Updating an existing ready source does not consume another slot.
	_, pending := beginResource(t, s, a.AgentID, "existing", "core", pkg.Info.Source)
	if err := s.FailResourceUpdate(ctx, a.AgentID, pending, "untrusted secret parser text"); err == nil {
		t.Fatal("arbitrary diagnostics persisted")
	}
	other, _ := newStore(t)
	_, b := create(t, other)
	if _, err := other.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: b.AgentID, ID: pending.Identity.ID}); err == nil {
		t.Fatal("profile saw another database update")
	}
	if _, err := s.db.Exec("UPDATE agents SET state='archived' WHERE id=?", a.AgentID); err != nil {
		t.Fatal(err)
	}
	r, _ = protocol.NewRequest("resource.register", "archived", protocol.RegisterResource{AgentID: a.AgentID, Kind: "skill", Path: pending.Identity.Source})
	if _, _, err := s.BeginResourceUpdate(ctx, r, a.AgentID, pending.Identity, "core"); err == nil {
		t.Fatal("archived Agent accepted preparation")
	}
}

func TestResourcePreparationRejectsUnboundedIdentityAndRollsBack(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, a := create(t, s)
	identity, _ := resource.Identity("skill", "/new/skill")
	req, _ := protocol.NewRequest("resource.register", "install", protocol.RegisterResource{AgentID: a.AgentID, Kind: "skill", Path: identity.Source})
	before, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	for _, params := range []struct{ op, owner string }{{"", "core"}, {strings.Repeat("x", 129), "core"}, {"install", ""}, {"install", strings.Repeat("x", 129)}} {
		altered := req
		altered.OperationID = params.op
		if _, _, err := s.BeginResourceUpdate(ctx, altered, a.AgentID, identity, params.owner); err == nil {
			t.Fatal("invalid preparation accepted")
		}
	}
	if _, err := s.db.Exec("CREATE TRIGGER fail_prepare BEFORE INSERT ON resource_updates BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginResourceUpdate(ctx, req, a.AgentID, identity, "core"); err == nil {
		t.Fatal("failed preparation accepted")
	}
	after, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("preparing event escaped failed transaction")
	}
	if _, err := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: identity.ID}); err == nil {
		t.Fatal("failed preparation created source status")
	}
}
