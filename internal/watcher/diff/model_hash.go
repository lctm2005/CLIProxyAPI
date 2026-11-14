package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// ComputeOpenAICompatModelsHash returns a stable hash for OpenAI-compat models.
// Used to detect model list changes during hot reload.
func ComputeOpenAICompatModelsHash(models []config.OpenAICompatibilityModel) string {
	return modelconfig.ComputeOpenAICompatModelsHash(models)
}

// ComputeVertexCompatModelsHash returns a stable hash for Vertex-compatible models.
func ComputeVertexCompatModelsHash(models []config.VertexCompatModel) string {
	return modelconfig.ComputeVertexCompatModelsHash(models)
}

// ComputeClaudeModelsHash returns a stable hash for Claude model aliases.
func ComputeClaudeModelsHash(models []config.ClaudeModel) string {
	return modelconfig.ComputeClaudeModelsHash(models)
}

// ComputeCodexModelsHash returns a stable hash for Codex model aliases.
func ComputeCodexModelsHash(models []config.CodexModel) string {
	return modelconfig.ComputeCodexModelsHash(models)
}

// ComputeGeminiModelsHash returns a stable hash for Gemini model aliases.
func ComputeGeminiModelsHash(models []config.GeminiModel) string {
	return modelconfig.ComputeGeminiModelsHash(models)
}

// ComputeTraeCLIModelsHash returns a stable hash for TRAE CLI model aliases.
func ComputeTraeCLIModelsHash(models []config.TraeCLIModel) string {
	keys := normalizeModelPairs(func(out func(key string)) {
		for _, model := range models {
			name := strings.TrimSpace(model.Name)
			alias := strings.TrimSpace(model.Alias)
			modelName := strings.TrimSpace(model.ModelName)
			aliases := normalizedTraeCLIAliases(model.Aliases)
			aliasNames := normalizedTraeCLIAliasNames(model.AliasNames)
			if name == "" && alias == "" && modelName == "" && len(aliases) == 0 && aliasNames == "" {
				continue
			}
			out(strings.ToLower(name) + "|" + strings.ToLower(alias) + "|" + strings.ToLower(modelName) + "|" + strings.Join(aliases, ",") + "|" + aliasNames)
		}
	})
	return hashJoined(keys)
}

func normalizedTraeCLIAliasNames(raw map[string]string) string {
	if len(raw) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(raw))
	for key, name := range raw {
		k := strings.ToLower(strings.TrimSpace(key))
		v := strings.TrimSpace(name)
		if k == "" || v == "" {
			continue
		}
		pairs = append(pairs, k+"="+v)
	}
	if len(pairs) == 0 {
		return ""
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func normalizedTraeCLIAliases(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	aliases := make([]string, 0, len(raw))
	for _, item := range raw {
		alias := strings.ToLower(strings.TrimSpace(item))
		if alias == "" {
			continue
		}
		if _, exists := seen[alias]; exists {
			continue
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}

// ComputeExcludedModelsHash returns a normalized hash for excluded model lists.
func ComputeExcludedModelsHash(excluded []string) string {
	if len(excluded) == 0 {
		return ""
	}
	normalized := make([]string, 0, len(excluded))
	for _, entry := range excluded {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			normalized = append(normalized, strings.ToLower(trimmed))
		}
	}
	if len(normalized) == 0 {
		return ""
	}
	sort.Strings(normalized)
	data, _ := json.Marshal(normalized)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func thinkingHashSuffix(support *registry.ThinkingSupport) string {
	data, _ := json.Marshal(support)
	return "|thinking=" + string(data)
}

func normalizeModelPairs(collect func(out func(key string))) []string {
	seen := make(map[string]struct{})
	keys := make([]string, 0)
	collect(func(key string) {
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	})
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	return keys
}

func hashJoined(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}
