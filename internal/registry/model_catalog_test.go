package registry

import "testing"

func TestUpdateClientModelsPreservesExistingCooldown(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client", "traecli", []*ModelInfo{{ID: "old"}, {ID: "removed"}})
	r.RegisterClient("other", "traecli", []*ModelInfo{{ID: "removed"}})
	r.SetModelQuotaExceeded("client", "old")
	r.SuspendClientModel("client", "old", "quota")
	quota := *r.models["old"].QuotaExceededClients["client"]
	r.UpdateClientModels("client", "traecli", []*ModelInfo{{ID: "old", ContextLength: 800000}, {ID: "new"}})
	if got := r.models["old"].QuotaExceededClients["client"]; got == nil || !got.Equal(quota) {
		t.Fatalf("catalog update reset quota: %v", got)
	}
	if got := r.models["old"].SuspendedClients["client"]; got != "quota" {
		t.Fatalf("catalog update reset suspension: %q", got)
	}
	if !r.ClientSupportsModel("client", "new") || r.ClientSupportsModel("client", "removed") {
		t.Fatal("model bindings were not reconciled")
	}
	if got := r.GetModelInfo("old", "traecli").ContextLength; got != 800000 {
		t.Fatalf("context metadata = %d", got)
	}
	r.SetModelQuotaExceeded("client", "removed")
	r.SuspendClientModel("client", "removed", "quota")
	if r.models["removed"].QuotaExceededClients["client"] != nil || r.models["removed"].SuspendedClients["client"] != "" {
		t.Fatal("late results restored removed client state")
	}
	r.RegisterClient("client", "traecli", []*ModelInfo{{ID: "old"}})
	if r.models["old"].QuotaExceededClients["client"] != nil || r.models["old"].SuspendedClients["client"] != "" {
		t.Fatal("ordinary registration no longer resets transient state")
	}
}
