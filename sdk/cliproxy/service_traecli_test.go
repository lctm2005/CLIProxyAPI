package cliproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestBuildTraeCLIModelsRetainsLastValidCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	service := &Service{cfg: &config.Config{TraeCLI: config.TraeCLIConfig{
		IncludeCacheModels: true, ModelsCache: path,
	}}}
	write := func(raw string) {
		t.Helper()
		if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	write(`{"models":[{"config_name":"cached-model","supported_in_api":true,"context_window":800000}]}`)
	if models := service.buildTraeCLIModels(nil); len(models) != 1 || models[0].ID != "cached-model" {
		t.Fatalf("initial catalog = %#v", models)
	}
	for _, raw := range []string{"", `{`, `{}`, `null`, `{"models":null}`, `{"models":{}}`, `{"models":[null]}`} {
		t.Run(raw, func(t *testing.T) {
			write(raw)
			models := service.buildTraeCLIModels(nil)
			if len(models) != 1 || models[0].ID != "cached-model" || models[0].ContextLength != 800000 {
				t.Fatalf("invalid cache replaced last valid catalog: %#v", models)
			}
		})
	}
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatal(errRemove)
	}
	if models := service.buildTraeCLIModels(nil); len(models) != 1 || models[0].ID != "cached-model" {
		t.Fatalf("missing cache replaced last valid catalog: %#v", models)
	}
	write(`{"models":[{"config_name":"recovered-model","supported_in_api":true}]}`)
	if models := service.buildTraeCLIModels(nil); len(models) != 1 || models[0].ID != "recovered-model" {
		t.Fatalf("valid replacement did not recover: %#v", models)
	}
}

func TestBuildTraeCLIModelsValidEmptyCacheIsAuthoritative(t *testing.T) {
	for _, raw := range []string{`{"models":[]}`, `{"models":[{"config_name":"hidden","supported_in_api":false}]}`} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "models_cache.json")
			if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			service := &Service{cfg: &config.Config{TraeCLI: config.TraeCLIConfig{ModelsCache: path, IncludeCacheModels: true}}}
			if models := service.buildTraeCLIModels(nil); len(models) != 0 {
				t.Fatalf("valid empty cache exposed fallback models: %#v", models)
			}
			service.cfg.TraeCLI.Models = []config.TraeCLIModel{{Name: "Explicit", Alias: "explicit-model"}}
			if models := service.buildTraeCLIModels(nil); len(models) != 1 || models[0].ID != "explicit-model" {
				t.Fatalf("valid empty cache removed explicit model: %#v", models)
			}
		})
	}
}

func TestBuildTraeCLIModelsMergesCacheWithConfiguredAliases(t *testing.T) {
	modelsCache := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"models":[
		{"slug":"GPT-5.5","config_name":"gpt-5.5","context_window":272000,"supported_in_api":true,"business_metadata":{"variants":{"standard_key":"gpt-5.5__dev","standard_context_window":272000,"max_key":"gpt-5.5__max","max_context_window":800000}}},
		{"slug":"Seed-2.1-Pro","config_name":"Doubao-Seed-2.1-Pro","context_window":184000,"supported_in_api":true,"business_metadata":{"variants":{"standard_key":"Doubao-Seed-2.1-Pro__dev","standard_context_window":184000,"max_key":"Doubao-Seed-2.1-Pro__max","max_context_window":800000}}},
		{"slug":"Kimi-K2.6","config_name":"kimi-k2.6","context_window":200000,"supported_in_api":true,"business_metadata":{"variants":{"standard_key":"kimi-k2.6__dev","standard_context_window":200000}}},
		{"slug":"Hidden","config_name":"hidden","context_window":200000,"supported_in_api":false}
	]}`)
	if errWrite := os.WriteFile(modelsCache, raw, 0o600); errWrite != nil {
		t.Fatalf("write models cache: %v", errWrite)
	}

	service := &Service{cfg: &config.Config{TraeCLI: config.TraeCLIConfig{
		IncludeCacheModels: true,
		ModelsCache:        modelsCache,
		BackendVariant:     "max",
		Models: []config.TraeCLIModel{
			{
				Name:      "GPT-5.5",
				Alias:     "gpt-5.5",
				Aliases:   []string{"claude-sonnet-5"},
				ModelName: "gpt-5.5__max",
			},
			{
				Name:      "Seed-2.1-Pro",
				Alias:     "seed-2.1-pro",
				ModelName: "Doubao-Seed-2.1-Pro__dev",
			},
		},
	}}}

	models := service.buildTraeCLIModels(nil)
	byID := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	for _, id := range []string{"gpt-5.5", "claude-sonnet-5", "seed-2.1-pro", "kimi-k2.6"} {
		if byID[id] == nil {
			t.Fatalf("missing model %q in %#v", id, models)
		}
	}
	for _, id := range []string{"Doubao-Seed-2.1-Pro", "hidden"} {
		if byID[id] != nil {
			t.Fatalf("unexpected model %q in %#v", id, models)
		}
	}
	if len(models) != 4 {
		t.Fatalf("model count = %d, want 4: %#v", len(models), models)
	}
	for _, id := range []string{"gpt-5.5", "claude-sonnet-5"} {
		if got := byID[id].ContextLength; got != 800000 {
			t.Fatalf("model %q context length = %d, want 800000", id, got)
		}
	}
	if got := byID["seed-2.1-pro"].ContextLength; got != 184000 {
		t.Fatalf("standard-variant model context length = %d, want 184000", got)
	}
}

func TestBuildTraeCLIModelsPreservesCacheModalitiesWithContextOverride(t *testing.T) {
	modelsCache := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"models":[
		{"slug":"Vision","config_name":"vision-model","context_window":200000,"supported_in_api":true,"input_modalities":["text","image"],"business_metadata":{"variants":{"standard_key":"vision-model__dev","standard_context_window":200000}}}
	]}`)
	if errWrite := os.WriteFile(modelsCache, raw, 0o600); errWrite != nil {
		t.Fatalf("write models cache: %v", errWrite)
	}

	service := &Service{cfg: &config.Config{TraeCLI: config.TraeCLIConfig{
		IncludeCacheModels: true,
		ModelsCache:        modelsCache,
		Models: []config.TraeCLIModel{{
			Name:          "Vision",
			Alias:         "vision-public",
			ModelName:     "vision-model__dev",
			ContextWindow: 123000,
		}},
	}}}

	models := service.buildTraeCLIModels(nil)
	if len(models) != 1 {
		t.Fatalf("model count = %d, want 1: %#v", len(models), models)
	}
	if got := strings.Join(models[0].SupportedInputModalities, ","); got != "text,image" {
		t.Fatalf("input modalities = %q, want text,image", got)
	}
	if got := models[0].ContextLength; got != 123000 {
		t.Fatalf("context length = %d, want explicit override 123000", got)
	}
}

func TestBuildTraeCLIModelsIncludesCanonicalAliases(t *testing.T) {
	service := &Service{cfg: &config.Config{TraeCLI: config.TraeCLIConfig{
		Models: []config.TraeCLIModel{{
			Name:      "GPT-5.4",
			Alias:     "gpt-5.4",
			Aliases:   []string{" claude-haiku-4-5 ", "CLAUDE-HAIKU-4-5", "gpt-5.4", ""},
			ModelName: "gpt-5.4__max",
		}},
	}}}

	models := service.buildTraeCLIModels(nil)
	if len(models) != 2 {
		t.Fatalf("model count = %d, want primary plus one canonical alias: %#v", len(models), models)
	}
	byID := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	primary := byID["gpt-5.4"]
	if primary == nil {
		t.Fatalf("missing primary model in %#v", models)
	}
	if primary.Type != "traecli" || primary.DisplayName != "GPT-5.4" {
		t.Fatalf("primary model = %#v, want Trae metadata with display name GPT-5.4", primary)
	}
	alias := byID["claude-haiku-4-5"]
	if alias == nil {
		t.Fatalf("missing canonical alias model in %#v", models)
	}
	if alias.Type != "traecli" {
		t.Fatalf("alias model type = %q, want traecli", alias.Type)
	}
	if alias.DisplayName != "claude-haiku-4-5" {
		t.Fatalf("alias display name = %q, want the canonical id so it does not collide with the primary", alias.DisplayName)
	}
}

func TestBuildTraeCLIModelsUsesAliasDisplayNames(t *testing.T) {
	service := &Service{cfg: &config.Config{TraeCLI: config.TraeCLIConfig{
		Models: []config.TraeCLIModel{{
			Name:      "GPT-5.4",
			Alias:     "gpt-5.4",
			Aliases:   []string{"claude-haiku-4-5"},
			ModelName: "gpt-5.4__max",
			AliasNames: map[string]string{
				" CLAUDE-HAIKU-4-5 ": "Claude Haiku (内部)",
			},
		}},
	}}}

	models := service.buildTraeCLIModels(nil)
	byID := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	primary := byID["gpt-5.4"]
	if primary == nil || primary.DisplayName != "GPT-5.4" {
		t.Fatalf("primary model = %#v, want display name GPT-5.4", primary)
	}
	alias := byID["claude-haiku-4-5"]
	if alias == nil {
		t.Fatalf("missing canonical alias model in %#v", models)
	}
	if alias.DisplayName != "Claude Haiku (内部)" {
		t.Fatalf("alias display name = %q, want the configured friendly name", alias.DisplayName)
	}
}
