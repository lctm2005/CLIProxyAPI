package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type traeCLIModelCacheVariants struct {
	StandardKey           string `json:"standard_key"`
	StandardContextWindow int    `json:"standard_context_window"`
	MaxKey                string `json:"max_key"`
	MaxContextWindow      int    `json:"max_context_window"`
}

type traeCLIModelCacheEntry struct {
	Slug               string   `json:"slug"`
	ConfigName         string   `json:"config_name"`
	Description        string   `json:"description"`
	ContextWindow      int      `json:"context_window"`
	SupportedInAPI     bool     `json:"supported_in_api"`
	InputModalities    []string `json:"input_modalities"`
	SupportedMimeTypes []string `json:"supported_mime_types"`
	BusinessMetadata   struct {
		Variants traeCLIModelCacheVariants `json:"variants"`
	} `json:"business_metadata"`
}

func (s *Service) buildTraeCLIModels(auth *coreauth.Auth) []*ModelInfo {
	cfg := &config.Config{}
	if s != nil {
		s.cfgMu.RLock()
		if s.cfg != nil {
			cfg = s.cfg
		}
		s.cfgMu.RUnlock()
	}
	var snapshot *traeCLIModelsSnapshot
	if s != nil && (len(cfg.TraeCLI.Models) == 0 || cfg.TraeCLI.IncludeCacheModels) {
		snapshot = s.traeModelsCache.load(context.Background(), traeCLIModelsCachePathForConfig(auth, cfg))
	}
	return s.buildTraeCLIModelsForAuth(auth, cfg, snapshot)
}

// Both registration and refresh use the same snapshot and model rules.
func (s *Service) buildTraeCLIModelsForAuth(auth *coreauth.Auth, cfg *config.Config, snapshot *traeCLIModelsSnapshot) []*ModelInfo {
	models := buildTraeCLIModelsFromSnapshot(cfg.TraeCLI.Models, snapshot, strings.TrimSpace(cfg.TraeCLI.BackendVariant))
	if auth == nil {
		return models
	}
	authKind := auth.AuthKind()
	excluded := cfg.OAuthExcludedModels["traecli"]
	if authKind == coreauth.AuthKindAPIKey {
		excluded = cfg.TraeCLI.ExcludedModels
	}
	// The account attribute contains the complete, pre-merged exclusions.
	if value := strings.TrimSpace(auth.Attributes["excluded_models"]); value != "" {
		excluded = strings.Split(value, ",")
	}
	models = applyExcludedModels(models, excluded)
	models = applyOAuthModelAliasForAuth(cfg, "traecli", authKind, auth.Attributes, models)
	models = s.appendPluginModels("traecli", models)
	return applyModelPrefixes(models, auth.Prefix, cfg.ForceModelPrefix)
}

func buildTraeCLIModelsFromSnapshot(entries []config.TraeCLIModel, snapshot *traeCLIModelsSnapshot, backendVariant string) []*ModelInfo {
	if snapshot == nil {
		if len(entries) > 0 {
			return buildConfiguredTraeCLIModels(entries)
		}
		return []*ModelInfo{defaultTraeCLIModel()}
	}
	cacheModels, cacheEntries := buildCachedTraeCLIModels(snapshot.entries, backendVariant)
	if len(entries) > 0 {
		return mergeConfiguredTraeCLIModels(cacheModels, cacheEntries, entries, backendVariant)
	}
	return cacheModels
}

func buildCachedTraeCLIModels(entries []traeCLIModelCacheEntry, backendVariant string) ([]*ModelInfo, map[string]traeCLIModelCacheEntry) {
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	cacheEntries := make(map[string]traeCLIModelCacheEntry, len(entries)*2)
	for _, entry := range entries {
		if !entry.SupportedInAPI {
			continue
		}
		id := strings.TrimSpace(entry.ConfigName)
		if id == "" {
			id = strings.TrimSpace(entry.Slug)
		}
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		cacheEntries[key] = entry
		if slugKey := strings.ToLower(strings.TrimSpace(entry.Slug)); slugKey != "" {
			if _, exists := cacheEntries[slugKey]; !exists {
				cacheEntries[slugKey] = entry
			}
		}
		contextWindow := traeCLIModelCacheContextWindow(entry, backendVariant)
		model := &ModelInfo{
			ID:               id,
			Object:           "model",
			Created:          now,
			OwnedBy:          "trae",
			Type:             "traecli",
			DisplayName:      strings.TrimSpace(entry.Slug),
			Description:      strings.TrimSpace(entry.Description),
			ContextLength:    contextWindow,
			MaxContextLength: contextWindow,
			UserDefined:      true,
		}
		if model.DisplayName == "" {
			model.DisplayName = id
		}
		if len(entry.InputModalities) > 0 {
			model.SupportedInputModalities = append([]string(nil), entry.InputModalities...)
		} else if len(entry.SupportedMimeTypes) > 0 {
			model.SupportedInputModalities = []string{"text", "image"}
		}
		out = append(out, model)
	}
	return out, cacheEntries
}

func traeCLIModelCacheContextWindow(entry traeCLIModelCacheEntry, backendVariant string) int {
	contextWindow := entry.ContextWindow
	variants := entry.BusinessMetadata.Variants
	if strings.EqualFold(backendVariant, "max") && strings.TrimSpace(variants.MaxKey) != "" {
		if variants.MaxContextWindow > 0 {
			return variants.MaxContextWindow
		}
	} else if strings.TrimSpace(variants.StandardKey) != "" && variants.StandardContextWindow > 0 {
		return variants.StandardContextWindow
	}
	return contextWindow
}

func traeCLIConfiguredBackendVariant(entry config.TraeCLIModel, fallback string) string {
	modelName := strings.TrimSpace(entry.ModelName)
	if variantIndex := strings.LastIndex(modelName, "__"); variantIndex > 0 {
		switch strings.ToLower(strings.TrimSpace(modelName[variantIndex+2:])) {
		case "max":
			return "max"
		case "dev", "standard":
			return ""
		}
	}
	return strings.TrimSpace(fallback)
}

func mergeConfiguredTraeCLIModels(cacheModels []*ModelInfo, cacheEntries map[string]traeCLIModelCacheEntry, entries []config.TraeCLIModel, backendVariant string) []*ModelInfo {
	configured := buildConfiguredTraeCLIModels(entries)
	if len(configured) == 0 {
		return cacheModels
	}

	cacheByID := make(map[string]*ModelInfo, len(cacheModels))
	for _, model := range cacheModels {
		if model != nil {
			cacheByID[strings.ToLower(strings.TrimSpace(model.ID))] = model
		}
	}
	configuredByID := make(map[string]*ModelInfo, len(configured))
	seen := make(map[string]struct{}, len(configured)+len(cacheModels))
	for _, model := range configured {
		if model == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(model.ID))
		configuredByID[key] = model
		seen[key] = struct{}{}
	}

	replacedCacheIDs := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		backendID := traeCLIConfiguredBackendID(entry)
		if backendID == "" {
			continue
		}
		backendKey := strings.ToLower(backendID)
		cacheKey := backendKey
		cacheEntry, hasCacheEntry := cacheEntries[backendKey]
		if hasCacheEntry {
			cacheKey = strings.ToLower(strings.TrimSpace(cacheEntry.ConfigName))
			if cacheKey == "" {
				cacheKey = strings.ToLower(strings.TrimSpace(cacheEntry.Slug))
			}
		}
		replacedCacheIDs[cacheKey] = struct{}{}
		cacheModel := cacheByID[cacheKey]
		if cacheModel == nil {
			continue
		}
		ids := append([]string{traeCLIConfiguredPrimaryID(entry)}, entry.Aliases...)
		for _, id := range ids {
			model := configuredByID[strings.ToLower(strings.TrimSpace(id))]
			if model == nil {
				continue
			}
			if entry.ContextWindow <= 0 {
				contextWindow := cacheModel.ContextLength
				if hasCacheEntry {
					contextWindow = traeCLIModelCacheContextWindow(cacheEntry, traeCLIConfiguredBackendVariant(entry, backendVariant))
				}
				model.ContextLength = contextWindow
				model.MaxContextLength = contextWindow
			}
			if len(model.SupportedInputModalities) == 0 {
				model.SupportedInputModalities = append([]string(nil), cacheModel.SupportedInputModalities...)
			}
		}
	}

	models := make([]*ModelInfo, 0, len(configured)+len(cacheModels))
	models = append(models, configured...)
	for _, model := range cacheModels {
		if model == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(model.ID))
		if _, exists := seen[key]; exists {
			continue
		}
		if _, replaced := replacedCacheIDs[key]; replaced {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}
	return models
}

func traeCLIConfiguredPrimaryID(entry config.TraeCLIModel) string {
	if alias := strings.TrimSpace(entry.Alias); alias != "" {
		return alias
	}
	return strings.TrimSpace(entry.Name)
}

func traeCLIConfiguredBackendID(entry config.TraeCLIModel) string {
	modelName := strings.TrimSpace(entry.ModelName)
	if modelName == "" {
		return traeCLIConfiguredPrimaryID(entry)
	}
	if variantIndex := strings.LastIndex(modelName, "__"); variantIndex > 0 {
		modelName = strings.TrimSpace(modelName[:variantIndex])
	}
	return modelName
}

func buildConfiguredTraeCLIModels(entries []config.TraeCLIModel) []*ModelInfo {
	primary := buildConfigModels(entries, "trae", "traecli")
	if len(primary) == 0 {
		return nil
	}

	byPrimaryID := make(map[string]*ModelInfo, len(primary))
	seen := make(map[string]struct{}, len(primary))
	models := make([]*ModelInfo, 0, len(primary))
	for _, model := range primary {
		if model == nil {
			continue
		}
		key := strings.ToLower(model.ID)
		seen[key] = struct{}{}
		byPrimaryID[key] = model
		models = append(models, model)
	}
	for _, entry := range entries {
		primaryID := strings.TrimSpace(entry.Alias)
		if primaryID == "" {
			primaryID = strings.TrimSpace(entry.Name)
		}
		base := byPrimaryID[strings.ToLower(primaryID)]
		if base == nil {
			continue
		}
		aliasDisplayNames := make(map[string]string, len(entry.AliasNames))
		for rawKey, rawName := range entry.AliasNames {
			key := strings.ToLower(strings.TrimSpace(rawKey))
			name := strings.TrimSpace(rawName)
			if key == "" || name == "" {
				continue
			}
			aliasDisplayNames[key] = name
		}
		for _, rawAlias := range entry.Aliases {
			alias := strings.TrimSpace(rawAlias)
			key := strings.ToLower(alias)
			if alias == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			clone := *base
			clone.ID = alias
			clone.DisplayName = alias
			if name, ok := aliasDisplayNames[key]; ok {
				clone.DisplayName = name
			}
			clone.Description = ""
			models = append(models, &clone)
			seen[key] = struct{}{}
		}
	}
	return models
}

func defaultTraeCLIModel() *ModelInfo {
	return &ModelInfo{
		ID:            "gpt-5.5",
		Object:        "model",
		Created:       time.Now().Unix(),
		OwnedBy:       "trae",
		Type:          "traecli",
		DisplayName:   "GPT-5.5",
		ContextLength: 272000,
		UserDefined:   true,
	}
}

func traeCLIModelsCachePathForConfig(auth *coreauth.Auth, cfg *config.Config) string {
	if auth != nil && auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["models_cache"]); value != "" {
			return expandTraeCLIServicePath(value)
		}
	}
	if cfg != nil {
		if value := strings.TrimSpace(cfg.TraeCLI.ModelsCache); value != "" {
			return expandTraeCLIServicePath(value)
		}
	}
	return expandTraeCLIServicePath("~/.trae/cli/models_cache.json")
}

func expandTraeCLIServicePath(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			rest := strings.TrimLeft(strings.TrimPrefix(path, "~"), `/\`)
			if rest == "" {
				return filepath.Clean(home)
			}
			return filepath.Clean(filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(rest, "\\", "/"))))
		}
	}
	return filepath.Clean(path)
}
