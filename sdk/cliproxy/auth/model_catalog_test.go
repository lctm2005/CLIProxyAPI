package auth

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestUpdateModelCatalogPreservesCooldownAndRefreshesScheduler(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	id := "trae-catalog-state"
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	if _, errRegister := m.Register(ctx, &Auth{ID: id, Provider: "traecli"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	reg.RegisterClient(id, "traecli", []*registry.ModelInfo{{ID: "old"}, {ID: "removed"}})
	m.RefreshSchedulerEntry(id)
	delay := 10 * time.Minute
	for _, model := range []string{"old", "removed"} {
		m.MarkResult(ctx, Result{AuthID: id, Provider: "traecli", Model: model, RetryAfter: &delay,
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}})
	}
	before, version := m.ModelCatalogSnapshot(id)
	if !m.UpdateModelCatalog(ctx, id, version, []*registry.ModelInfo{{ID: "old"}, {ID: "new"}}) {
		t.Fatal("catalog update was rejected")
	}
	after, _ := m.GetByID(id)
	if !after.ModelStates["old"].NextRetryAfter.Equal(before.ModelStates["old"].NextRetryAfter) || !after.ModelStates["old"].Quota.Exceeded {
		t.Fatal("catalog update cleared existing cooldown")
	}
	if after.ModelStates["removed"] != nil {
		t.Fatal("removed model retained runtime state")
	}
	if _, errPick := m.scheduler.pickSingle(ctx, "traecli", "new", cliproxyexecutor.Options{}, nil); errPick != nil {
		t.Fatalf("new model was not schedulable: %v", errPick)
	}
	if _, errPick := m.scheduler.pickSingle(ctx, "traecli", "old", cliproxyexecutor.Options{}, nil); errPick == nil {
		t.Fatal("catalog update made the cooling model schedulable")
	}
	m.MarkResult(ctx, Result{AuthID: id, Provider: "traecli", Model: "removed", Error: &Error{HTTPStatus: 429}})
	after, _ = m.GetByID(id)
	if after.ModelStates["removed"] != nil {
		t.Fatal("late result restored removed model state")
	}
}

func TestUpdateModelCatalogRejectsReplacedAuth(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	id := "trae-catalog-replacement"
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	auth := &Auth{ID: id, Provider: "traecli"}
	if _, errRegister := m.Register(ctx, auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	_, version := m.ModelCatalogSnapshot(id)
	m.Remove(ctx, id)
	if _, errRegister := m.Register(ctx, auth.Clone()); errRegister != nil {
		t.Fatal(errRegister)
	}
	if m.UpdateModelCatalog(ctx, id, version, []*registry.ModelInfo{{ID: "stale"}}) {
		t.Fatal("catalog from the removed auth updated its replacement")
	}
	auth, version = m.ModelCatalogSnapshot(id)
	auth.Disabled = true
	if _, errUpdate := m.Update(ctx, auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if m.UpdateModelCatalog(ctx, id, version, []*registry.ModelInfo{{ID: "stale"}}) {
		t.Fatal("catalog update restored a disabled auth")
	}
}

func TestUpdateModelCatalogRejectsOlderCatalogAndRemovedAttempt(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	id := t.Name()
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	if _, errRegister := m.Register(ctx, &Auth{ID: id, Provider: "traecli"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	reg.RegisterClient(id, "traecli", []*registry.ModelInfo{{ID: "model"}})
	generation := m.nextResultGeneration()
	_, version := m.ModelCatalogSnapshot(id)
	if !m.UpdateModelCatalog(ctx, id, version, nil) {
		t.Fatal("removal failed")
	}
	if m.UpdateModelCatalog(ctx, id, version, []*registry.ModelInfo{{ID: "stale"}}) {
		t.Fatal("older catalog overwrote removal")
	}
	_, version = m.ModelCatalogSnapshot(id)
	if !m.UpdateModelCatalog(ctx, id, version, []*registry.ModelInfo{{ID: "model"}}) {
		t.Fatal("re-add failed")
	}
	m.markResult(ctx, Result{AuthID: id, Model: "model", Error: &Error{HTTPStatus: 429}}, generation)
	auth, _ := m.GetByID(id)
	if auth.ModelStates["model"] != nil {
		t.Fatal("an old attempt restored state after re-add")
	}
	m.markResult(ctx, Result{AuthID: id, Model: "model", Error: &Error{HTTPStatus: 429}}, m.nextResultGeneration())
	auth, _ = m.GetByID(id)
	if auth.ModelStates["model"] == nil || !auth.ModelStates["model"].Quota.Exceeded {
		t.Fatal("new attempt did not record cooldown")
	}
	_, version = m.ModelCatalogSnapshot(id)
	if !m.UpdateModelCatalog(ctx, id, version, nil) {
		t.Fatal("second removal failed")
	}
	reg.RegisterClient(id, "traecli", []*registry.ModelInfo{{ID: "model"}})
	m.ReconcileRegistryModelStates(ctx, id)
	m.MarkResult(ctx, Result{AuthID: id, Model: "model", Error: &Error{HTTPStatus: 429}})
	auth, _ = m.GetByID(id)
	if auth.ModelStates["model"] == nil {
		t.Fatal("ordinary re-registration retained a removal marker")
	}
}

func TestUpdateModelCatalogConcurrentResults(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	id := t.Name()
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	if _, errRegister := m.Register(ctx, &Auth{ID: id, Provider: "traecli"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	reg.RegisterClient(id, "traecli", []*registry.ModelInfo{{ID: "retained"}})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		delay := 10 * time.Minute
		for i := 0; i < 50; i++ {
			m.MarkResult(ctx, Result{AuthID: id, Model: "retained", RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
		}
	}()
	for i := 0; i < 50; i++ {
		_, version := m.ModelCatalogSnapshot(id)
		models := []*registry.ModelInfo{{ID: "retained", ContextLength: i + 1}}
		if i%2 == 0 {
			models = append(models, &registry.ModelInfo{ID: "changing"})
		}
		if !m.UpdateModelCatalog(ctx, id, version, models) {
			t.Error("concurrent result rejected catalog update")
		}
	}
	wg.Wait()
	auth, _ := m.GetByID(id)
	state := auth.ModelStates["retained"]
	if state == nil || !state.Quota.Exceeded || !state.NextRetryAfter.After(time.Now().Add(9*time.Minute)) {
		t.Fatal("catalog update lost the concurrent cooldown")
	}
	if _, errPick := m.scheduler.pickSingle(ctx, "traecli", "retained", cliproxyexecutor.Options{}, nil); errPick == nil {
		t.Fatal("scheduler lost the concurrent cooldown")
	}
}

func TestUpdateModelCatalogRetainsCredentialQuota(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	id := t.Name()
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(id) })
	if _, errRegister := m.Register(ctx, &Auth{ID: id, Provider: "traecli"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	reg.RegisterClient(id, "traecli", []*registry.ModelInfo{{ID: "removed"}})
	delay := 10 * time.Minute
	m.MarkResult(ctx, Result{AuthID: id, Model: "removed", RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
	before, version := m.ModelCatalogSnapshot(id)
	if !m.UpdateModelCatalog(ctx, id, version, []*registry.ModelInfo{{ID: "added"}}) {
		t.Fatal("catalog update rejected")
	}
	after, _ := m.GetByID(id)
	if !reflect.DeepEqual(after.Quota, before.Quota) || !after.Unavailable {
		t.Fatal("catalog update cleared credential-wide quota")
	}
	if _, errPick := m.scheduler.pickSingle(ctx, "traecli", "added", cliproxyexecutor.Options{}, nil); errPick == nil {
		t.Fatal("new model bypassed credential-wide cooldown")
	}
}

func TestStaleAuthUpdateLeavesModelCatalogUsable(t *testing.T) {
	for _, mode := range []string{"replace", "refresh", "prepare"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(nil, nil, nil)
			id := t.Name()
			reg := registry.GetGlobalRegistry()
			t.Cleanup(func() { reg.UnregisterClient(id) })
			old, errRegister := m.Register(ctx, &Auth{ID: id, Provider: "traecli"})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			m.Remove(ctx, id)
			if _, errRegister = m.Register(ctx, &Auth{ID: id, Provider: "traecli"}); errRegister != nil {
				t.Fatal(errRegister)
			}
			var errUpdate error
			switch mode {
			case "replace":
				_, errUpdate = m.Update(ctx, old.Clone())
			case "refresh":
				_, errUpdate = m.UpdateRefreshedAuth(ctx, old, old.Clone())
			case "prepare":
				_, errUpdate = m.UpdatePreparedAuth(ctx, old, old.Clone())
			}
			if errUpdate == nil {
				t.Fatal("stale auth update was accepted")
			}
			_, version := m.ModelCatalogSnapshot(id)
			done := make(chan bool, 1)
			go func() {
				done <- m.UpdateModelCatalog(ctx, id, version, []*registry.ModelInfo{{ID: "new-model"}})
			}()
			select {
			case accepted := <-done:
				if !accepted {
					t.Fatal("current catalog update was rejected")
				}
			case <-time.After(time.Second):
				t.Fatal("catalog update blocked after stale auth rejection")
			}
		})
	}
}
