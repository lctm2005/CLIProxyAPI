package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newTraeRefreshFixture(t *testing.T) (*Service, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	id := t.Name()
	s := &Service{cfg: &config.Config{TraeCLI: config.TraeCLIConfig{Enabled: true, IncludeCacheModels: true, ModelsCache: path}}, coreManager: coreauth.NewManager(nil, nil, nil)}
	if _, errRegister := s.coreManager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "traecli", Status: coreauth.StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	t.Cleanup(func() { s.stopTraeCLIModelRefresh(); GlobalModelRegistry().UnregisterClient(id) })
	return s, id, path
}

func writeTraeRefreshCache(t *testing.T, path, raw string) {
	t.Helper()
	if errWrite := os.WriteFile(path+".next", []byte(raw), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errRename := os.Rename(path+".next", path); errRename != nil {
		t.Fatal(errRename)
	}
}

func traeRefreshModelIDs(id string) []string {
	var ids []string
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(id) {
		ids = append(ids, model.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestTraeModelRefreshUpdatesCatalogAndIgnoresNonSemanticChanges(t *testing.T) {
	s, id, path := newTraeRefreshFixture(t)
	ctx := context.Background()
	writeTraeRefreshCache(t, path, `{"models":[{"config_name":"a","supported_in_api":true,"context_window":800000}]}`)
	if n := s.syncTraeCLIModels(ctx); n != 1 {
		t.Fatalf("initial updates = %d", n)
	}
	delay := 10 * time.Minute
	s.coreManager.MarkResult(ctx, coreauth.Result{AuthID: id, Model: "a", RetryAfter: &delay, Error: &coreauth.Error{HTTPStatus: 429}})
	before, _ := s.coreManager.GetByID(id)
	writeTraeRefreshCache(t, path, `{"models":[{"config_name":"a","supported_in_api":true,"context_window":900000},{"config_name":"b","supported_in_api":true}]}`)
	if n := s.syncTraeCLIModels(ctx); n != 1 || !reflect.DeepEqual(traeRefreshModelIDs(id), []string{"a", "b"}) {
		t.Fatalf("new catalog = %v, updates = %d", traeRefreshModelIDs(id), n)
	}
	after, _ := s.coreManager.GetByID(id)
	if !after.ModelStates["a"].NextRetryAfter.Equal(before.ModelStates["a"].NextRetryAfter) {
		t.Fatal("refresh changed the cooldown")
	}
	if got := registry.GetGlobalRegistry().GetModelInfo("a", "traecli"); got == nil || got.ContextLength != 900000 {
		t.Fatalf("updated model metadata = %#v", got)
	}
	writeTraeRefreshCache(t, path, `{"updated_at":"later","models":[{"config_name":"b","supported_in_api":true},{"config_name":"a","supported_in_api":true,"context_window":900000}]}`)
	if n := s.syncTraeCLIModels(ctx); n != 0 {
		t.Fatalf("reordered catalog caused %d updates", n)
	}
	for _, raw := range []string{"", "{", `{"models":null}`} {
		writeTraeRefreshCache(t, path, raw)
		if n := s.syncTraeCLIModels(ctx); n != 0 {
			t.Fatalf("invalid cache caused %d updates", n)
		}
	}
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatal(errRemove)
	}
	if n := s.syncTraeCLIModels(ctx); n != 0 {
		t.Fatalf("missing cache caused %d updates", n)
	}
	writeTraeRefreshCache(t, path, `{"models":[]}`)
	if n := s.syncTraeCLIModels(ctx); n != 1 || len(traeRefreshModelIDs(id)) != 0 {
		t.Fatal("valid empty cache did not remove dynamic models")
	}
	writeTraeRefreshCache(t, path, `{"models":[{"config_name":"recovered","supported_in_api":true}]}`)
	if n := s.syncTraeCLIModels(ctx); n != 1 || !reflect.DeepEqual(traeRefreshModelIDs(id), []string{"recovered"}) {
		t.Fatal("cache recreation did not recover")
	}
}

func TestTraeModelRefreshUsesSnapshotAndRejectsStaleCandidates(t *testing.T) {
	for _, change := range []string{"file", "config", "disabled", "removed", "replacement"} {
		t.Run(change, func(t *testing.T) {
			s, id, path := newTraeRefreshFixture(t)
			ctx := context.Background()
			writeTraeRefreshCache(t, path, `{"models":[{"config_name":"snapshot","supported_in_api":true}]}`)
			updates := s.collectTraeCLIModelUpdates(ctx)
			if len(updates) != 1 {
				t.Fatalf("candidate count = %d", len(updates))
			}
			auth, _ := s.coreManager.GetByID(id)
			switch change {
			case "file":
				writeTraeRefreshCache(t, path, `{"models":[{"config_name":"later","supported_in_api":true}]}`)
			case "config":
				cfg := *s.cfg
				cfg.TraeCLI.ModelsCache = path + ".new"
				s.commitConfigUpdate(&cfg)
			case "disabled":
				auth.Disabled = true
				if _, errUpdate := s.coreManager.Update(ctx, auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			case "removed", "replacement":
				s.coreManager.Remove(ctx, id)
				if change == "replacement" {
					if _, errRegister := s.coreManager.Register(ctx, auth); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
			}
			published := s.publishTraeCLIModelUpdate(ctx, updates[0])
			if change == "file" {
				if !published || !reflect.DeepEqual(traeRefreshModelIDs(id), []string{"snapshot"}) {
					t.Fatal("publication reread a different file")
				}
			} else if published || len(traeRefreshModelIDs(id)) != 0 {
				t.Fatal("stale candidate was published")
			}
		})
	}
}

func TestTraeModelRefreshRespectsConfiguration(t *testing.T) {
	s, id, path := newTraeRefreshFixture(t)
	ctx := context.Background()
	s.cfg.TraeCLI.Models = []config.TraeCLIModel{{Name: "a", Alias: "alias", ModelName: "a"}}
	s.cfg.OAuthExcludedModels = map[string][]string{"traecli": {"hidden"}}
	s.cfg.ForceModelPrefix = true
	auth, _ := s.coreManager.GetByID(id)
	auth.Prefix = "local"
	if _, errUpdate := s.coreManager.Update(ctx, auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	writeTraeRefreshCache(t, path, `{"models":[{"config_name":"a","supported_in_api":true},{"config_name":"b","supported_in_api":true},{"config_name":"hidden","supported_in_api":true},{"config_name":"unsupported","supported_in_api":false}]}`)
	s.syncTraeCLIModels(ctx)
	if got := traeRefreshModelIDs(id); !reflect.DeepEqual(got, []string{"local/alias", "local/b"}) {
		t.Fatalf("configured models = %v", got)
	}
	writeTraeRefreshCache(t, path, `{"models":[]}`)
	s.syncTraeCLIModels(ctx)
	if got := traeRefreshModelIDs(id); !reflect.DeepEqual(got, []string{"local/alias"}) {
		t.Fatalf("explicit models were lost: %v", got)
	}
	writeTraeRefreshCache(t, path, `{"models":[{"config_name":"new","supported_in_api":true}]}`)
	s.cfg.TraeCLI.IncludeCacheModels = false
	if n := s.syncTraeCLIModels(ctx); n != 0 {
		t.Fatal("explicit-only configuration polled the cache")
	}
	s.cfg.TraeCLI.IncludeCacheModels = true
	s.cfg.Home.Enabled = true
	if n := s.syncTraeCLIModels(ctx); n != 0 {
		t.Fatal("Home catalog was overwritten")
	}
}

func TestTraeModelRefreshMatchesRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, kind, excluded string
		want                 []string
	}{
		{"API key config exclusions", coreauth.AuthKindAPIKey, "", []string{"local/alias", "local/oauth-hidden"}},
		{"OAuth config exclusions", "oauth", "", []string{"local/alias", "local/config-hidden"}},
		{"API key account exclusions", coreauth.AuthKindAPIKey, "*-hidden", []string{"local/alias"}},
		{"OAuth account exclusions", "oauth", "*-hidden", []string{"local/alias"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id, path := newTraeRefreshFixture(t)
			ctx := context.Background()
			s.cfg.TraeCLI.Models = []config.TraeCLIModel{{Name: "a", Alias: "alias", ModelName: "a"}}
			s.cfg.TraeCLI.ExcludedModels = []string{"config-*"}
			s.cfg.OAuthExcludedModels = map[string][]string{"traecli": {"oauth-*"}}
			s.cfg.ForceModelPrefix = true
			auth, _ := s.coreManager.GetByID(id)
			auth.Prefix = "local"
			auth.Attributes = map[string]string{coreauth.AttributeAuthKind: tc.kind, "excluded_models": tc.excluded}
			if _, errUpdate := s.coreManager.Update(ctx, auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			writeTraeRefreshCache(t, path, `{"models":[{"config_name":"a","supported_in_api":true,"context_window":800000},{"config_name":"config-hidden","supported_in_api":true},{"config_name":"oauth-hidden","supported_in_api":true}]}`)
			s.registerModelsForAuth(ctx, auth)
			if got := traeRefreshModelIDs(id); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("registered models = %v, want %v", got, tc.want)
			}
			if n := s.syncTraeCLIModels(ctx); n != 0 {
				t.Fatalf("unchanged cache caused %d updates: %v", n, traeRefreshModelIDs(id))
			}
			writeTraeRefreshCache(t, path, `{"models":[{"config_name":"a","supported_in_api":true,"context_window":900000},{"config_name":"config-hidden","supported_in_api":true},{"config_name":"oauth-hidden","supported_in_api":true}]}`)
			if n := s.syncTraeCLIModels(ctx); n != 1 {
				t.Fatalf("metadata change caused %d updates", n)
			}
			if got := traeRefreshModelIDs(id); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("refreshed models = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTraeModelRefreshLifecycle(t *testing.T) {
	s, id, path := newTraeRefreshFixture(t)
	writeTraeRefreshCache(t, path, `{"models":[{"config_name":"initial","supported_in_api":true}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startTraeCLIModelRefresh(ctx, 10*time.Millisecond)
	waitForModel := func(model string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if GlobalModelRegistry().ClientSupportsModel(id, model) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("poller did not publish %s", model)
	}
	waitForModel("initial")
	writeTraeRefreshCache(t, path, `{"models":[{"config_name":"next","supported_in_api":true}]}`)
	waitForModel("next")
	cancel()
	s.stopTraeCLIModelRefresh()
	writeTraeRefreshCache(t, path, `{"models":[]}`)
	if n := s.syncTraeCLIModels(ctx); n != 0 {
		t.Fatal("cancelled refresh published models")
	}
	if !GlobalModelRegistry().ClientSupportsModel(id, "next") {
		t.Fatal("stopped poller changed the catalog")
	}
}

func TestTraeModelRefreshCachePathChanges(t *testing.T) {
	s, id, configPath := newTraeRefreshFixture(t)
	ctx := context.Background()
	// Without explicit models, the cache remains the discovery source even
	// when include-cache-models is false.
	s.cfg.TraeCLI.IncludeCacheModels = false
	writeTraeRefreshCache(t, configPath, `{"models":[{"config_name":"config-model","supported_in_api":true}]}`)
	authPath := configPath + ".auth"
	writeTraeRefreshCache(t, authPath, `{"models":[{"config_name":"auth-model","supported_in_api":true},{"config_name":"excluded-model","supported_in_api":true}]}`)
	auth, _ := s.coreManager.GetByID(id)
	auth.Attributes = map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey, "models_cache": authPath, "excluded_models": "excluded-*"}
	if _, errUpdate := s.coreManager.Update(ctx, auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	s.syncTraeCLIModels(ctx)
	if got := traeRefreshModelIDs(id); !reflect.DeepEqual(got, []string{"auth-model"}) {
		t.Fatalf("auth cache/exclusion rules = %v", got)
	}
	newPath := configPath + ".new"
	writeTraeRefreshCache(t, newPath, `{"models":[{"config_name":"new-config-model","supported_in_api":true}]}`)
	cfg := *s.cfg
	cfg.TraeCLI.ModelsCache = newPath
	s.commitConfigUpdate(&cfg)
	if n := s.syncTraeCLIModels(ctx); n != 0 {
		t.Fatal("config path overrode the account path")
	}
	delete(auth.Attributes, "models_cache")
	if _, errUpdate := s.coreManager.Update(ctx, auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	s.syncTraeCLIModels(ctx)
	if got := traeRefreshModelIDs(id); !reflect.DeepEqual(got, []string{"new-config-model"}) {
		t.Fatalf("new config path did not take effect: %v", got)
	}
	// A malformed old source must use its own snapshot, not the newer path's.
	writeTraeRefreshCache(t, authPath, `{`)
	auth.Attributes["models_cache"] = authPath
	if _, errUpdate := s.coreManager.Update(ctx, auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	s.syncTraeCLIModels(ctx)
	if got := traeRefreshModelIDs(id); !reflect.DeepEqual(got, []string{"auth-model"}) {
		t.Fatalf("last valid catalog crossed source paths: %v", got)
	}
}
