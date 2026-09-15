package synthesizer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/diff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// synthesizeTraeCLI creates a local Auth entry for TRAE CLI execution.
func (s *ConfigSynthesizer) synthesizeTraeCLI(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	if cfg == nil || !cfg.TraeCLI.Enabled {
		return nil
	}

	authFile := strings.TrimSpace(cfg.TraeCLI.AuthFile)
	modelsCache := strings.TrimSpace(cfg.TraeCLI.ModelsCache)
	baseURL := strings.TrimSpace(cfg.TraeCLI.BaseURL)
	id, token := ctx.IDGenerator.Next("traecli:local", authFile, modelsCache, baseURL)
	attrs := map[string]string{
		coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey,
		coreauth.AttributeAPIKey:   "local-traecli-auth",
		"source":                   fmt.Sprintf("config:traecli[%s]", token),
	}
	if cfg.TraeCLI.Priority != 0 {
		attrs["priority"] = strconv.Itoa(cfg.TraeCLI.Priority)
	}
	addTraeCLIAttr(attrs, "mode", cfg.TraeCLI.Mode)
	addTraeCLIAttr(attrs, "auth_file", authFile)
	addTraeCLIAttr(attrs, "models_cache", modelsCache)
	addTraeCLIAttr(attrs, "base_url", baseURL)
	addTraeCLIAttr(attrs, "app_id", cfg.TraeCLI.AppID)
	addTraeCLIAttr(attrs, "function", cfg.TraeCLI.Function)
	addTraeCLIAttr(attrs, "backend_variant", cfg.TraeCLI.BackendVariant)
	if cfg.TraeCLI.IncludeCacheModels {
		attrs["include_cache_models"] = "true"
	}
	addTraeCLIAttr(attrs, "preset_file", cfg.TraeCLI.PresetFile)
	if cfg.TraeCLI.NativeFallbackToExec {
		attrs["native_fallback_to_exec"] = "true"
	}
	if cfg.TraeCLI.NativeFallbackCooldownSeconds != 0 {
		attrs["native_fallback_cooldown_seconds"] = strconv.Itoa(cfg.TraeCLI.NativeFallbackCooldownSeconds)
	}
	addTraeCLIAttr(attrs, "exec_path", cfg.TraeCLI.ExecPath)
	addTraeCLIAttr(attrs, "exec_workdir", cfg.TraeCLI.ExecWorkDir)
	addTraeCLIAttr(attrs, "exec_sandbox", cfg.TraeCLI.ExecSandbox)
	addTraeCLIAttr(attrs, "exec_permission_mode", cfg.TraeCLI.ExecPermission)
	addTraeCLIJSONAttr(attrs, "exec_extra_args", cfg.TraeCLI.ExecExtraArgs)
	addTraeCLIJSONAttr(attrs, "exec_env", cfg.TraeCLI.ExecEnv)
	if cfg.TraeCLI.ExecInheritShellEnv {
		attrs["exec_inherit_shell_env"] = "true"
	}
	addTraeCLIAttr(attrs, "exec_shell", cfg.TraeCLI.ExecShell)
	if hash := diff.ComputeTraeCLIModelsHash(cfg.TraeCLI.Models); hash != "" {
		attrs["models_hash"] = hash
	}
	addConfigHeadersToAttrs(cfg.TraeCLI.Headers, attrs)

	metadata := map[string]any{}
	if cfg.TraeCLI.DisableCooling {
		metadata["disable_cooling"] = true
	}
	auth := &coreauth.Auth{
		ID:         id,
		Provider:   "traecli",
		Label:      "traecli-local",
		Prefix:     strings.TrimSpace(cfg.TraeCLI.Prefix),
		Status:     coreauth.StatusActive,
		Attributes: attrs,
		Metadata:   metadata,
		CreatedAt:  ctx.Now,
		UpdatedAt:  ctx.Now,
	}
	ApplyAuthExcludedModelsMeta(auth, cfg, cfg.TraeCLI.ExcludedModels, "apikey")
	if len(auth.Metadata) == 0 {
		auth.Metadata = nil
	}
	return []*coreauth.Auth{auth}
}

func addTraeCLIAttr(attrs map[string]string, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		attrs[key] = value
	}
}

func addTraeCLIJSONAttr(attrs map[string]string, key string, value any) {
	raw, err := json.Marshal(value)
	if err == nil && string(raw) != "null" && string(raw) != "[]" && string(raw) != "{}" {
		attrs[key] = string(raw)
	}
}
