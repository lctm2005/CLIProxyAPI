package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestTraeCLIExecutorBuildRawChatBody(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		BackendVariant: "max",
		ModelsCache:    writeTraeCLIModelsCache(t, ""),
	}})
	body, err := exec.buildRawChatBody(nil, []byte(`{
		"model":"gpt-5.5",
		"messages":[
			{"role":"system","content":"system prompt"},
			{"role":"user","content":[{"type":"text","text":"hello"}]}
		],
		"max_tokens":16,
		"reasoning_effort":"medium"
	}`), []byte(`{"system":[{"type":"text","text":"claude system"}]}`), "gpt-5.5")
	if err != nil {
		t.Fatalf("buildRawChatBody error = %v", err)
	}
	if got := gjson.GetBytes(body, "config_name").String(); got != "gpt-5.5" {
		t.Fatalf("config_name = %q, want gpt-5.5", got)
	}
	if got := gjson.GetBytes(body, "model_name").String(); got != "gpt-5.5__max" {
		t.Fatalf("model_name = %q, want gpt-5.5__max", got)
	}
	if got := gjson.GetBytes(body, "reasoning_effort").String(); got != "medium" {
		t.Fatalf("reasoning_effort = %q, want medium", got)
	}
	if got := gjson.GetBytes(body, "user_input").String(); got != "hello" {
		t.Fatalf("user_input = %q, want hello", got)
	}
	if got := gjson.GetBytes(body, "messages.0.content.0.text").String(); got != "claude system" {
		t.Fatalf("messages system text = %q, want claude system", got)
	}
	if got := gjson.GetBytes(body, "messages.2.content.0.text").String(); got != "hello" {
		t.Fatalf("messages user text = %q, want hello", got)
	}
	if got := gjson.GetBytes(body, "max_tokens").Int(); got != 16 {
		t.Fatalf("max_tokens = %d, want 16", got)
	}
}

func TestTraeCLIExecutorBuildRawChatBodyUsesModelMetadata(t *testing.T) {
	modelsCache := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"models":[{"slug":"Seed-2.1-Turbo","config_name":"Doubao-Seed-2.1-Turbo","default_reasoning_level":"high","supported_reasoning_levels":[]}]}`)
	if err := os.WriteFile(modelsCache, raw, 0o600); err != nil {
		t.Fatalf("WriteFile models cache: %v", err)
	}
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		BackendVariant: "max",
		ModelsCache:    modelsCache,
		Models: []config.TraeCLIModel{{
			Name:      "05 Seed-2.1-Turbo",
			Alias:     "seed-2.1-turbo",
			ModelName: "Doubao-Seed-2.1-Turbo__dev",
		}},
	}})
	body, err := exec.buildRawChatBody(nil, []byte(`{
		"model":"seed-2.1-turbo",
		"messages":[{"role":"user","content":"hello"}],
		"reasoning_effort":"xhigh"
	}`), nil, "seed-2.1-turbo")
	if err != nil {
		t.Fatalf("buildRawChatBody error = %v", err)
	}
	if got := gjson.GetBytes(body, "config_name").String(); got != "Doubao-Seed-2.1-Turbo" {
		t.Fatalf("config_name = %q, want Doubao-Seed-2.1-Turbo", got)
	}
	if got := gjson.GetBytes(body, "model_name").String(); got != "Doubao-Seed-2.1-Turbo__dev" {
		t.Fatalf("model_name = %q, want Doubao-Seed-2.1-Turbo__dev", got)
	}
	if got := gjson.GetBytes(body, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want high", got)
	}
}

func TestTraeCLIExecutorBuildRawChatBodyUsesAvailableCacheVariant(t *testing.T) {
	modelsCache := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"models":[
		{"slug":"Kimi-K2.6","config_name":"kimi-k2.6","business_metadata":{"variants":{"standard_key":"kimi-k2.6__dev","max_key":null}}},
		{"slug":"Seed-Code","config_name":"Doubao-Seed-Code","business_metadata":{"variants":{"standard_key":"Doubao-Seed-Code__dev","max_key":"Doubao-Seed-Code__max"}}}
	]}`)
	if errWrite := os.WriteFile(modelsCache, raw, 0o600); errWrite != nil {
		t.Fatalf("write models cache: %v", errWrite)
	}
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		BackendVariant: "max",
		ModelsCache:    modelsCache,
	}})

	for model, want := range map[string]string{
		"kimi-k2.6":        "kimi-k2.6__dev",
		"Doubao-Seed-Code": "Doubao-Seed-Code__max",
	} {
		if got := exec.rawModelName(nil, model); got != want {
			t.Fatalf("rawModelName(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestTraeCLIExecutorBuildRawChatBodyResolvesCanonicalAlias(t *testing.T) {
	modelsCache := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"models":[{"slug":"GPT-5.4","config_name":"gpt-5.4","base_instructions":"TRAE base instructions","default_reasoning_level":"xhigh","supported_reasoning_levels":[{"effort":"xhigh"}]}]}`)
	if err := os.WriteFile(modelsCache, raw, 0o600); err != nil {
		t.Fatalf("WriteFile models cache: %v", err)
	}
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		ModelsCache: modelsCache,
		Models: []config.TraeCLIModel{{
			Name:      "GPT-5.4",
			Alias:     "gpt-5.4",
			Aliases:   []string{"claude-haiku-4-5"},
			ModelName: "gpt-5.4__max",
		}},
	}})
	body, err := exec.buildRawChatBody(nil, []byte(`{
		"model":"claude-haiku-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"reasoning_effort":"xhigh"
	}`), nil, "claude-haiku-4-5")
	if err != nil {
		t.Fatalf("buildRawChatBody error = %v", err)
	}
	if got := gjson.GetBytes(body, "config_name").String(); got != "gpt-5.4" {
		t.Fatalf("config_name = %q, want gpt-5.4", got)
	}
	if got := gjson.GetBytes(body, "model_name").String(); got != "gpt-5.4__max" {
		t.Fatalf("model_name = %q, want gpt-5.4__max", got)
	}
	if got := exec.baseInstructions(nil, "claude-haiku-4-5"); got != "TRAE base instructions" {
		t.Fatalf("baseInstructions = %q, want TRAE base instructions", got)
	}
}

func TestTraeCLIExecutorPrimaryAliasWinsAdditionalAliasCollision(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		Models: []config.TraeCLIModel{
			{
				Name:      "Backend A",
				Alias:     "backend-a",
				Aliases:   []string{"shared-model"},
				ModelName: "backend-a__max",
			},
			{
				Name:      "Backend B",
				Alias:     "shared-model",
				ModelName: "backend-b__max",
			},
		},
	}})

	if got := exec.rawModelName(nil, "shared-model"); got != "backend-b__max" {
		t.Fatalf("rawModelName(shared-model) = %q, want primary alias backend-b__max", got)
	}
}

func TestTraeCLIExecutorBuildRawChatBodyResolvesCanonicalAliasWithoutModelName(t *testing.T) {
	modelsCache := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"models":[{"slug":"GPT-5.4","config_name":"gpt-5.4","base_instructions":"TRAE base instructions","default_reasoning_level":"xhigh","supported_reasoning_levels":[{"effort":"xhigh"}]}]}`)
	if err := os.WriteFile(modelsCache, raw, 0o600); err != nil {
		t.Fatalf("WriteFile models cache: %v", err)
	}
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		ModelsCache:    modelsCache,
		BackendVariant: "max",
		Models: []config.TraeCLIModel{{
			Name:    "GPT-5.4",
			Alias:   "gpt-5.4",
			Aliases: []string{"claude-haiku-4-5"},
		}},
	}})
	body, err := exec.buildRawChatBody(nil, []byte(`{
		"model":"claude-haiku-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"reasoning_effort":"xhigh"
	}`), nil, "claude-haiku-4-5")
	if err != nil {
		t.Fatalf("buildRawChatBody error = %v", err)
	}
	if got := gjson.GetBytes(body, "config_name").String(); got != "gpt-5.4" {
		t.Fatalf("config_name = %q, want gpt-5.4", got)
	}
	if got := gjson.GetBytes(body, "model_name").String(); got != "gpt-5.4__max" {
		t.Fatalf("model_name = %q, want gpt-5.4__max", got)
	}
	if got := exec.baseInstructions(nil, "claude-haiku-4-5"); got != "TRAE base instructions" {
		t.Fatalf("baseInstructions = %q, want TRAE base instructions", got)
	}
}

func TestTraeCLIExecutorBuildRawChatBodyResolvesMetadataFromMappedModelName(t *testing.T) {
	modelsCache := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"models":[{"slug":"openrouter-3o","config_name":"openrouter-3o","base_instructions":"TRAE base instructions","default_reasoning_level":"xhigh","supported_reasoning_levels":[{"effort":"xhigh"}]}]}`)
	if err := os.WriteFile(modelsCache, raw, 0o600); err != nil {
		t.Fatalf("WriteFile models cache: %v", err)
	}
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		ModelsCache: modelsCache,
		Models: []config.TraeCLIModel{{
			Name:      "Claude auto classifier",
			Alias:     "claude-opus-5",
			ModelName: "openrouter-3o__max",
		}},
	}})
	body, err := exec.buildRawChatBody(nil, []byte(`{
		"model":"claude-opus-5",
		"messages":[{"role":"user","content":"hello"}],
		"reasoning_effort":"xhigh"
	}`), nil, "claude-opus-5")
	if err != nil {
		t.Fatalf("buildRawChatBody error = %v", err)
	}
	if got := gjson.GetBytes(body, "config_name").String(); got != "openrouter-3o" {
		t.Fatalf("config_name = %q, want openrouter-3o", got)
	}
	if got := gjson.GetBytes(body, "model_name").String(); got != "openrouter-3o__max" {
		t.Fatalf("model_name = %q, want openrouter-3o__max", got)
	}
	if got := exec.baseInstructions(nil, "claude-opus-5"); got != "TRAE base instructions" {
		t.Fatalf("baseInstructions = %q, want TRAE base instructions", got)
	}
}

func TestTraeCLIExecutorBuildRawChatBodyCapsCacheControls(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		ModelsCache: writeTraeCLIModelsCache(t, ""),
	}})
	body, err := exec.buildRawChatBody(nil, []byte(`{
		"model":"gpt-5.5",
		"messages":[
			{"role":"user","content":"turn 1"},
			{"role":"assistant","content":"turn 2"},
			{"role":"user","content":"turn 3"},
			{"role":"assistant","content":"turn 4"},
			{"role":"user","content":"turn 5"},
			{"role":"assistant","content":"turn 6"}
		]
	}`), nil, "gpt-5.5")
	if err != nil {
		t.Fatalf("buildRawChatBody error = %v", err)
	}
	cacheControlCount := 0
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		for _, content := range message.Get("content").Array() {
			if content.Get("cache_control").Exists() {
				cacheControlCount++
			}
		}
	}
	if cacheControlCount != traeCLIMaxCacheControls {
		t.Fatalf("cache_control count = %d, want %d; body=%s", cacheControlCount, traeCLIMaxCacheControls, body)
	}
	for _, messageIndex := range []int{0, 1} {
		path := "messages." + strconv.Itoa(messageIndex) + ".content.0.cache_control"
		if gjson.GetBytes(body, path).Exists() {
			t.Fatalf("%s should be removed; body=%s", path, body)
		}
	}
	for _, messageIndex := range []int{2, 3, 4, 5} {
		path := "messages." + strconv.Itoa(messageIndex) + ".content.0.cache_control.type"
		if got := gjson.GetBytes(body, path).String(); got != "ephemeral" {
			t.Fatalf("%s = %q, want ephemeral; body=%s", path, got, body)
		}
	}
}

func TestTraeCLIOpenAIChatMessagesToTraeRawToolMessage(t *testing.T) {
	raw := openAIChatMessagesToTraeRaw([]byte(`{
		"messages":[
			{"role":"tool","tool_call_id":"call_123","content":"tool output"}
		]
	}`), nil, "")
	if got := gjson.GetBytes(raw, "0.role").String(); got != "tool" {
		t.Fatalf("role = %q, want tool; raw=%s", got, raw)
	}
	if got := gjson.GetBytes(raw, "0.tool_call_id").String(); got != "call_123" {
		t.Fatalf("tool_call_id = %q, want call_123; raw=%s", got, raw)
	}
	if got := gjson.GetBytes(raw, "0.content.0.type").String(); got != "text" {
		t.Fatalf("content.0.type = %q, want text; raw=%s", got, raw)
	}
	if got := gjson.GetBytes(raw, "0.content.0.text").String(); got != "tool output" {
		t.Fatalf("content.0.text = %q, want tool output; raw=%s", got, raw)
	}
}

func TestTraeCLILatestOpenAIUserTextSkipsToolResult(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"user","content":"first prompt"},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_123","type":"function","function":{"name":"pwd","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_123","content":"tool output"}
		]
	}`)
	if got := latestOpenAIUserText(raw); got != "first prompt" {
		t.Fatalf("latestOpenAIUserText() = %q, want first prompt", got)
	}
}

func TestTraeCLIOpenAIChatMessagesToTraeRawAssistantToolCalls(t *testing.T) {
	raw := openAIChatMessagesToTraeRaw([]byte(`{
		"messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_123","type":"function","function":{"name":"pwd","arguments":"{}"}}]}
		]
	}`), nil, "")
	if got := gjson.GetBytes(raw, "0.tool_calls.0.id").String(); got != "call_123" {
		t.Fatalf("tool call id = %q, want call_123; raw=%s", got, raw)
	}
	if got := gjson.GetBytes(raw, "0.tool_calls.0.function_call.name").String(); got != "pwd" {
		t.Fatalf("function_call.name = %q, want pwd; raw=%s", got, raw)
	}
	if got := gjson.GetBytes(raw, "0.tool_calls.0.function_call.arguments").String(); got != "{}" {
		t.Fatalf("function_call.arguments = %q, want {}; raw=%s", got, raw)
	}
	if gjson.GetBytes(raw, "0.tool_calls.0.function").Exists() {
		t.Fatalf("OpenAI function field should not be forwarded to raw-chat; raw=%s", raw)
	}
}

func TestTraeCLIExecutorBuildRawChatBodyAddsBaseInstructions(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		ModelsCache: writeTraeCLIModelsCache(t, "TRAE base instructions"),
	}})
	body, err := exec.buildRawChatBody(nil, []byte(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"hello"}]
	}`), []byte(`{"system":[{"type":"text","text":"client system"}]}`), "gpt-5.5")
	if err != nil {
		t.Fatalf("buildRawChatBody error = %v", err)
	}
	if got := gjson.GetBytes(body, "messages.0.content.0.text").String(); got != "TRAE base instructions" {
		t.Fatalf("base instructions = %q, want injected; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "messages.0.content.1.text").String(); got != "client system" {
		t.Fatalf("client system = %q, want preserved; body=%s", got, body)
	}
}

func TestTraeCLIExecutorPresetPreservesNativeContext(t *testing.T) {
	presetPath := filepath.Join(t.TempDir(), "preset.json")
	preset := []byte(`{
		"config_name":"gpt-5.5",
		"model_name":"gpt-5.5__max",
		"user_input":"old",
		"messages":[
			{"role":"system","content":[{"type":"text","text":"base"}]},
			{"role":"user","content":[{"type":"text","text":"permissions","response_api_role":"developer"}]},
			{"role":"user","content":[{"type":"text","text":"AGENTS"}]},
			{"role":"user","content":[{"type":"text","text":"old"}]}
		],
		"tools":[{"type":"function","function":{"name":"exec_command","parameters":"{}"}}],
		"biz_context":{"repo_urls":["https://github.com/router-for-me/CLIProxyAPI"]}
	}`)
	if err := os.WriteFile(presetPath, preset, 0o600); err != nil {
		t.Fatalf("WriteFile preset: %v", err)
	}
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{PresetFile: presetPath}})
	body, err := exec.buildRawChatBody(nil, []byte(`{"messages":[{"role":"user","content":"new prompt"}]}`), nil, "gpt-5.5")
	if err != nil {
		t.Fatalf("buildRawChatBody error = %v", err)
	}
	if got := gjson.GetBytes(body, "messages.#").Int(); got != 4 {
		t.Fatalf("messages count = %d, want 4; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "messages.1.content.0.text").String(); got != "permissions" {
		t.Fatalf("context message lost, got %q; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "messages.2.content.0.text").String(); got != "AGENTS" {
		t.Fatalf("AGENTS message lost, got %q; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "messages.3.content.0.text").String(); got != "new prompt" {
		t.Fatalf("last user = %q, want new prompt; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tools.0.function.name").String(); got != "exec_command" {
		t.Fatalf("tools lost, got %q; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "biz_context.repo_urls.0").String(); got != "https://github.com/router-for-me/CLIProxyAPI" {
		t.Fatalf("biz_context lost, got %q; body=%s", got, body)
	}
}

func TestTraeCLIExecutorBuildRawChatBodySkipsToolsByDefault(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		BackendVariant: "max",
		ModelsCache:    writeTraeCLIModelsCache(t, ""),
	}})
	sessionID := "11111111-1111-4111-8111-111111111111"
	translated := []byte(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"Read","description":"read file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true
	}`)
	original := []byte(`{
		"metadata":{"user_id":"{\"session_id\":\"` + sessionID + `\"}"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)
	body, sessionKey, err := exec.buildRawChatBodyForRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: original,
	}, cliproxyexecutor.Options{
		OriginalRequest: original,
		SourceFormat:    sdktranslator.FormatClaude,
	}, translated, original, "gpt-5.5")
	if err != nil {
		t.Fatalf("buildRawChatBodyForRequest error = %v", err)
	}
	if gjson.GetBytes(body, "tools").Exists() {
		t.Fatalf("tools should be skipped by default; body=%s", body)
	}
	if gjson.GetBytes(body, "functionDeclarations").Exists() {
		t.Fatalf("functionDeclarations should be skipped by default; body=%s", body)
	}
	if gjson.GetBytes(body, "tool_choice").Exists() {
		t.Fatalf("tool_choice should be skipped by default; body=%s", body)
	}
	if gjson.GetBytes(body, "toolConfig").Exists() {
		t.Fatalf("toolConfig should be skipped by default; body=%s", body)
	}
	if got := gjson.GetBytes(body, "parallel_tool_calls").Bool(); !got {
		t.Fatalf("parallel_tool_calls = false, want true; body=%s", body)
	}
	if got := gjson.GetBytes(body, "conversation_id").String(); got != sessionID {
		t.Fatalf("conversation_id = %q, want %q; body=%s", got, sessionID, body)
	}
	rawSessionID := gjson.GetBytes(body, "session_id").String()
	if rawSessionID == "" || rawSessionID == sessionID {
		t.Fatalf("session_id = %q, want non-empty per-request id distinct from conversation; body=%s", rawSessionID, body)
	}
	if sessionKey != sessionID {
		t.Fatalf("session key = %q, want %q", sessionKey, sessionID)
	}
}

func TestTraeCLIExecutorBuildRawChatBodyForwardToolsOptIn(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		Headers:     map[string]string{"x-traecli-forward-tools": "true"},
		ModelsCache: writeTraeCLIModelsCache(t, ""),
	}})
	body, _, err := exec.buildRawChatBodyForRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"session_id":"tool-session"}`),
	}, cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"session_id":"tool-session"}`),
	}, []byte(`{
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"Read","description":"read file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],
		"tool_choice":"auto"
	}`), []byte(`{"session_id":"tool-session"}`), "gpt-5.5")
	if err != nil {
		t.Fatalf("buildRawChatBodyForRequest error = %v", err)
	}
	if got := gjson.GetBytes(body, "tools.0.function.name").String(); got != "Read" {
		t.Fatalf("tools.0.function.name = %q, want Read; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tools.0.function.parameters").Type; got != gjson.String {
		t.Fatalf("tools.0.function.parameters type = %v, want string; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tools.0.function.parameters").String(); !strings.Contains(got, `"path"`) {
		t.Fatalf("tools.0.function.parameters = %q, want serialized schema; body=%s", got, body)
	}
	if gjson.GetBytes(body, "functionDeclarations").Exists() {
		t.Fatalf("functionDeclarations should not be sent to raw-chat; body=%s", body)
	}
	if gjson.GetBytes(body, "toolConfig").Exists() {
		t.Fatalf("toolConfig should not be sent to raw-chat; body=%s", body)
	}
}

func TestTraeCLIExecutorRawChatIdentityHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.enterprise.trae.cn/api/ide/v2/llm_raw_chat", nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	body := []byte(`{"session_id":"11111111-1111-4111-8111-111111111111","conversation_id":"22222222-2222-4222-8222-222222222222","user_input":"hello","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	applyTraeCLIRequestIdentityHeaders(req, body)
	if got := req.Header.Get("Session_id"); got != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("Session_id = %q", got)
	}
	if got := req.Header.Get("Conversation_id"); got != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("Conversation_id = %q", got)
	}
	if got := req.Header.Get("Thread-Id"); got != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("Thread-Id = %q", got)
	}
	firstRequestID := req.Header.Get("X-Client-Request-Id")
	if firstRequestID == "" {
		t.Fatal("X-Client-Request-Id is empty")
	}
	req2, _ := http.NewRequest(http.MethodPost, "https://api.enterprise.trae.cn/api/ide/v2/llm_raw_chat", nil)
	applyTraeCLIRequestIdentityHeaders(req2, body)
	if got := req2.Header.Get("X-Client-Request-Id"); got != firstRequestID {
		t.Fatalf("stable X-Client-Request-Id = %q, want %q", got, firstRequestID)
	}
}

func TestTraeCLIExecutorRepoHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.enterprise.trae.cn/api/ide/v2/llm_raw_chat", nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	applyTraeCLIRepoHeaders(req, []byte(`{"biz_context":{"repo_urls":["https://github.com/router-for-me/CLIProxyAPI","https://example.test/repo"]}}`))
	if got := req.Header.Get("X-Custom-Repo-Urls"); got != "https://github.com/router-for-me/CLIProxyAPI,https://example.test/repo" {
		t.Fatalf("X-Custom-Repo-Urls = %q", got)
	}
}

func TestTraeCLIExecutorExtraInfoReplay(t *testing.T) {
	resetTraeCLIExtraInfoCacheForTest()
	sessionID := "11111111-1111-4111-8111-111111111111"
	cacheTraeCLIExtraInfoBestEffort(sessionID, []byte(`{"opaque":"state"}`), traeCLIRequestOrder.Add(1))

	exec := NewTraeCLIExecutor(&config.Config{})
	body, _, err := exec.buildRawChatBodyForRequest(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"session_id":"` + sessionID + `"}`),
	}, cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"session_id":"` + sessionID + `"}`),
	}, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), []byte(`{"session_id":"`+sessionID+`"}`), "gpt-5.5")
	if err != nil {
		t.Fatalf("buildRawChatBodyForRequest error = %v", err)
	}
	if got := gjson.GetBytes(body, "extra_info.opaque").String(); got != "state" {
		t.Fatalf("extra_info.opaque = %q, want state; body=%s", got, body)
	}
}

func TestTraeCLIExecutorPrepareRequest(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/api/ide/v2/llm_raw_chat", nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	exec := NewTraeCLIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"auth_file": "testdata/trae_auth.json"}}
	if err := exec.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest error = %v", err)
	}
	if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Cloud-CLI-JWT ") {
		t.Fatalf("Authorization = %q, want Cloud-CLI-JWT prefix", got)
	}
	if got := req.Header.Get("X-App-Id"); got != traeCLIDefaultAppID {
		t.Fatalf("X-App-Id = %q, want default", got)
	}
	if got := req.Header.Get("X-IDE-Function"); got != traeCLIDefaultFunction {
		t.Fatalf("X-IDE-Function = %q, want default", got)
	}
	if got := req.Header.Get("User-Agent"); !strings.Contains(got, "xterm-256color") {
		t.Fatalf("User-Agent = %q, want xterm-256color terminal marker", got)
	}
}

func TestTraeCLIExecutorHTTPRequestSetsNativeModeHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	exec := NewTraeCLIExecutor(&config.Config{})
	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	resp, err := exec.HttpRequest(context.Background(), &cliproxyauth.Auth{Attributes: map[string]string{
		"auth_file": "testdata/trae_auth.json",
	}}, req)
	if err != nil {
		t.Fatalf("HttpRequest error = %v", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Errorf("response body close error = %v", errClose)
		}
	}()
	if got := resp.Header.Get("X-TraeCLI-Mode"); got != traeCLIModeNative {
		t.Fatalf("X-TraeCLI-Mode = %q, want %q", got, traeCLIModeNative)
	}
}

func TestTraeCLIExecutorMapsUpstreamRateLimitTo429(t *testing.T) {
	const responseBody = `{"type":"error","error":{"type":"api_error","message":"retryable error: quota exceeded, original: received error while streaming: {\"type\":\"too_many_requests\",\"code\":\"rate_limit_reached\",\"message\":\"Requests have exceeded the throughput limit on your Provisioned-Managed deployment.\"}"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, responseBody)
	}))
	defer server.Close()

	payload := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: payload}
	opts := cliproxyexecutor.Options{OriginalRequest: payload, SourceFormat: sdktranslator.FormatOpenAI}
	tests := []struct {
		name    string
		execute func(*TraeCLIExecutor) error
	}{
		{
			name: "non-stream",
			execute: func(exec *TraeCLIExecutor) error {
				_, err := exec.Execute(context.Background(), nil, req, opts)
				return err
			},
		},
		{
			name: "stream",
			execute: func(exec *TraeCLIExecutor) error {
				_, err := exec.ExecuteStream(context.Background(), nil, req, opts)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
				AuthFile:    "testdata/trae_auth.json",
				BaseURL:     server.URL,
				ModelsCache: writeTraeCLIModelsCache(t, ""),
			}})
			err := tt.execute(exec)
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusTooManyRequests {
				t.Fatalf("Execute error = %v (%T), want status %d", err, err, http.StatusTooManyRequests)
			}
		})
	}
}

func TestTraeCLIExecutorExecuteStreamForwardsContentBeforeUpstreamCompletes(t *testing.T) {
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseResponse)
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("ReadAll request body error = %v", errRead)
			return
		}
		if got := r.Header.Get("Session_id"); got == "" {
			t.Error("Session_id header is empty")
		}
		if got := r.Header.Get("Conversation_id"); got == "" {
			t.Error("Conversation_id header is empty")
		}
		if got := r.Header.Get("X-Client-Request-Id"); got == "" {
			t.Error("X-Client-Request-Id header is empty")
		}
		if got := gjson.GetBytes(body, "messages.0.content.0.text").String(); got != "hello" {
			t.Errorf("request message = %q, want hello; body=%s", got, body)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: request_wait_in_queue\ndata: {\"position\":20,\"message\":\"upstream status one\",\"queue_id\":\"queue-1\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-releaseResponse:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "event: request_wait_in_queue\ndata: {\"position\":10,\"message\":\"upstream status two\",\"queue_id\":\"queue-1\"}\n\n")
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"hel\",\"phase\":\"final_answer\"}\n\n")
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"lo\",\"phase\":null}\n\n")
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function_call\":{\"name\":\"Read\",\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}]}\n\n")
		_, _ = io.WriteString(w, "event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n")
	}))
	defer server.Close()
	defer release()

	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		AuthFile:    "testdata/trae_auth.json",
		BaseURL:     server.URL,
		ModelsCache: writeTraeCLIModelsCache(t, ""),
	}})
	payload := []byte(`{
		"model":"gpt-5.5",
		"max_tokens":1024,
		"stream":true,
		"conversation_id":"11111111-1111-4111-8111-111111111111",
		"messages":[{"role":"user","content":"hello"}]
	}`)
	result, err := exec.ExecuteStream(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error = %v", err)
	}
	if got := result.Headers.Get("X-TraeCLI-Mode"); got != traeCLIModeNative {
		t.Fatalf("X-TraeCLI-Mode = %q, want %q", got, traeCLIModeNative)
	}

	var initial strings.Builder
	firstChunkTimeout := time.NewTimer(2 * time.Second)
	defer firstChunkTimeout.Stop()
	for !strings.Contains(initial.String(), "upstream status one") {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				t.Fatalf("stream closed before first content; chunks=%q", initial.String())
			}
			if chunk.Err != nil {
				t.Fatalf("initial streamed chunk error = %v", chunk.Err)
			}
			initial.Write(chunk.Payload)
		case <-firstChunkTimeout.C:
			t.Fatalf("timed out waiting for first content; chunks=%q", initial.String())
		}
	}

	release()
	var remaining strings.Builder
	for {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				if !strings.Contains(remaining.String(), "upstream status two") || !strings.Contains(remaining.String(), "hel") || !strings.Contains(remaining.String(), "lo") {
					t.Fatalf("remaining chunks = %q, want refreshed status and final content", remaining.String())
				}
				if !strings.Contains(remaining.String(), "Read") || !strings.Contains(remaining.String(), "tool_use") {
					t.Fatalf("remaining chunks = %q, want streamed tool call", remaining.String())
				}
				return
			}
			if chunk.Err != nil {
				t.Fatalf("streamed chunk error = %v", chunk.Err)
			}
			remaining.Write(chunk.Payload)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for stream completion")
		}
	}
}

func TestParseTraeCLIRawChatPayload(t *testing.T) {
	raw := []byte("event: chat\n" +
		"data: {\"delta\":\"hel\"}\n\n" +
		"data: {\"data\":{\"content\":\"lo\"},\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n")
	text, usage, err := parseTraeCLIRawChatPayload(raw)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayload error = %v", err)
	}
	if text != "hello" {
		t.Fatalf("text = %q, want hello", text)
	}
	if usage.PromptTokens != 2 || usage.CompletionTokens != 3 || usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want 2/3/5", usage)
	}
}

func TestParseTraeCLIRawChatPayloadPreservesDetailedUsage(t *testing.T) {
	raw := []byte("data: {\"token_usage\":{\"input_tokens\":100,\"cached_input_tokens\":70,\"cache_creation_input_tokens\":5,\"output_tokens\":8,\"reasoning_output_tokens\":12,\"total_tokens\":120}}\n\n")
	_, usage, err := parseTraeCLIRawChatPayload(raw)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayload error = %v", err)
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
		t.Fatalf("usage totals = %+v, want 100/20/120", usage)
	}
	if usage.CachedTokens != 70 || usage.CacheCreationTokens != 5 || usage.ReasoningTokens != 12 {
		t.Fatalf("usage details = %+v, want cached=70 creation=5 reasoning=12", usage)
	}
}

func TestParseTraeCLIRawChatPayloadTreatsNestedReasoningAsOutputSubset(t *testing.T) {
	raw := []byte("data: {\"token_usage\":{\"input_tokens\":100,\"output_tokens\":20,\"output_tokens_details\":{\"reasoning_tokens\":12},\"total_tokens\":120}}\n\n")
	_, usage, err := parseTraeCLIRawChatPayload(raw)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayload error = %v", err)
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
		t.Fatalf("usage totals = %+v, want 100/20/120", usage)
	}
	if usage.ReasoningTokens != 12 {
		t.Fatalf("reasoning tokens = %d, want 12", usage.ReasoningTokens)
	}
}

func TestParseTraeCLIRawChatPayloadUsesLatestCumulativeUsage(t *testing.T) {
	raw := []byte("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n" +
		"data: {\"token_usage\":{\"prompt_tokens\":10,\"input_tokens\":999,\"completion_tokens\":5,\"output_tokens\":999,\"total_tokens\":15}}\n\n")
	_, usage, err := parseTraeCLIRawChatPayload(raw)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayload error = %v", err)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want latest cumulative snapshot 10/5/15", usage)
	}
}

func TestBuildTraeCLIChatCompletionFinishChunkIncludesUsage(t *testing.T) {
	chunk := buildTraeCLIChatCompletionFinishChunk("gpt-5.4", "stop", openAIUsage{
		PromptTokens:        10,
		CompletionTokens:    5,
		TotalTokens:         15,
		CachedTokens:        7,
		CacheCreationTokens: 2,
		ReasoningTokens:     3,
	}, true)
	data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(chunk), []byte("data:")))
	if got := gjson.GetBytes(data, "usage.prompt_tokens").Int(); got != 10 {
		t.Fatalf("prompt_tokens = %d, want 10; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(data, "usage.completion_tokens").Int(); got != 5 {
		t.Fatalf("completion_tokens = %d, want 5; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(data, "usage.total_tokens").Int(); got != 15 {
		t.Fatalf("total_tokens = %d, want 15; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(data, "usage.prompt_tokens_details.cached_tokens").Int(); got != 7 {
		t.Fatalf("cached_tokens = %d, want 7; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(data, "usage.prompt_tokens_details.cached_creation_tokens").Int(); got != 2 {
		t.Fatalf("cached_creation_tokens = %d, want 2; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(data, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 3 {
		t.Fatalf("reasoning_tokens = %d, want 3; chunk=%s", got, chunk)
	}
}

func TestStreamRawChatResultTranslatesUsageToClaude(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{})
	result := exec.streamRawChatResult(context.Background(), "gpt-5.4", cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
		OriginalRequest: []byte(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	}, []byte(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hello"}]}`), nil, traeCLIRawChatPayload{
		Text: "ok",
		Usage: openAIUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
		UsageSeen: true,
	})

	var output strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	stream := output.String()
	if count := strings.Count(stream, `"type":"message_delta"`); count != 1 {
		t.Fatalf("message_delta count = %d, want 1; stream=%s", count, stream)
	}
	if !strings.Contains(stream, `"usage":{"input_tokens":10,"output_tokens":5}`) {
		t.Fatalf("stream missing translated Claude usage: %s", stream)
	}
	if !strings.Contains(stream, `"type":"message_stop"`) {
		t.Fatalf("stream missing message_stop: %s", stream)
	}
}

func TestTraeCLIRawChatCacheReadUsageReachesClaude(t *testing.T) {
	// Preserve the top-level usage shape observed on the native TRAE endpoint.
	raw := []byte("data: {\"delta\":\"ok\"}\n\n" +
		"event: token_usage\n" +
		`data: {"prompt_tokens":27238,"completion_tokens":37,"total_tokens":27275,"reasoning_tokens":27,"cache_read_input_tokens":26880,"cache_creation_input_tokens":0,"prompt_tokens_total":0,"completion_tokens_total":0,"total_tokens_total":0,"reasoning_tokens_total":0,"cache_read_input_tokens_total":0,"cache_creation_input_tokens_total":0}` + "\n\n")
	parsed, errParse := parseTraeCLIRawChatPayloadDetailed(raw, nil)
	if errParse != nil {
		t.Fatalf("parseTraeCLIRawChatPayloadDetailed error = %v", errParse)
	}
	request := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	assertUsage := func(t *testing.T, usage gjson.Result) {
		t.Helper()
		for path, want := range map[string]int64{
			"cache_read_input_tokens":               26880,
			"input_tokens":                          358,
			"output_tokens":                         37,
			"output_tokens_details.thinking_tokens": 27,
		} {
			if got := usage.Get(path).Int(); got != want {
				t.Errorf("%s = %d, want %d; usage=%s", path, got, want, usage.Raw)
			}
		}
	}
	t.Run("stream", func(t *testing.T) {
		exec := NewTraeCLIExecutor(&config.Config{})
		streamRequest := []byte(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
		result := exec.streamRawChatResult(context.Background(), "gpt-5.5", cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatClaude,
			OriginalRequest: streamRequest,
		}, streamRequest, nil, parsed)
		var output strings.Builder
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
			output.Write(chunk.Payload)
		}
		for _, line := range strings.Split(output.String(), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if event.Get("type").String() == "message_delta" {
				assertUsage(t, event.Get("usage"))
				return
			}
		}
		t.Fatalf("missing message_delta: %s", output.String())
	})
	t.Run("nonstream", func(t *testing.T) {
		openAIResponse, errBuild := buildTraeCLIOpenAIResponse("gpt-5.5", parsed)
		if errBuild != nil {
			t.Fatalf("buildTraeCLIOpenAIResponse error = %v", errBuild)
		}
		var param any
		response := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "gpt-5.5", request, request, openAIResponse, &param)
		assertUsage(t, gjson.GetBytes(response, "usage"))
	})
}

func TestTraeCLIEmbeddedRateLimitErrorMapsTo429(t *testing.T) {
	raw := []byte("event: error\n" +
		"data: {\"code\":\"UPSTREAM_ERROR\",\"message\":\"retryable error: quota exceeded, original: {\\\"type\\\":\\\"too_many_requests\\\",\\\"code\\\":\\\"rate_limit_reached\\\"}\"}\n\n")
	tests := []struct {
		name  string
		parse func() error
	}{
		{name: "status event", parse: func() error { return traeCLIStatusError(raw) }},
		{name: "stream accumulator", parse: func() error {
			_, _, err := parseTraeCLIRawChatPayload(raw)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.parse()
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusTooManyRequests {
				t.Fatalf("parse error = %v (%T), want status %d", err, err, http.StatusTooManyRequests)
			}
		})
	}
}

func TestTraeCLIUpstreamErrorStatusCodePreservesUnrelatedErrors(t *testing.T) {
	if got := traeCLIUpstreamErrorStatusCode(http.StatusBadGateway, []byte(`{"code":"UPSTREAM_ERROR","message":"temporary unavailable"}`)); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", got, http.StatusBadGateway)
	}
	if got := traeCLIUpstreamErrorStatusCode(http.StatusUnauthorized, []byte(`{"error":"invalid token"}`)); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got, http.StatusUnauthorized)
	}
}

func TestTraeCLIUpstreamErrorStatusCodeClassifiesObservedFailures(t *testing.T) {
	tests := []struct {
		name     string
		fallback int
		body     string
		want     int
	}{
		{
			name:     "parameter error wrapped as bad gateway",
			fallback: http.StatusBadGateway,
			body:     `{"type":"error","error":{"type":"api_error","message":"We're sorry, the param is invalid. Please try with a valid param."}}`,
			want:     http.StatusBadRequest,
		},
		{
			name:     "unauthorized parameter message keeps status",
			fallback: http.StatusUnauthorized,
			body:     `{"type":"error","error":{"type":"api_error","message":"The token param is invalid."}}`,
			want:     http.StatusUnauthorized,
		},
		{
			name:     "unstructured bad gateway parameter message keeps status",
			fallback: http.StatusBadGateway,
			body:     `upstream says the param is invalid`,
			want:     http.StatusBadGateway,
		},
		{
			name:     "non api bad gateway parameter message keeps status",
			fallback: http.StatusBadGateway,
			body:     `{"type":"error","error":{"type":"authentication_error","message":"The token param is invalid."}}`,
			want:     http.StatusBadGateway,
		},
		{
			name:     "nonstandard parameter response normalizes to bad gateway",
			fallback: 515,
			body:     `{"type":"error","error":{"type":"api_error","message":"The param is invalid."}}`,
			want:     http.StatusBadGateway,
		},
		{
			name:     "nonstandard upstream server status",
			fallback: 515,
			body:     `status 515`,
			want:     http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := traeCLIUpstreamErrorStatusCode(tt.fallback, []byte(tt.body)); got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseTraeCLIRawChatPayloadOutputEvent(t *testing.T) {
	raw := []byte("event: metadata\n" +
		"data: {\"model\":\"gpt-5.5\"}\n\n" +
		"event: output\n" +
		"data: {\"response\":\"OK\",\"phase\":\"final_answer\"}\n\n" +
		"event: token_usage\n" +
		"data: {\"prompt_tokens\":10641,\"completion_tokens\":22,\"total_tokens\":10663,\"reasoning_tokens\":15}\n\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"stop\"}\n\n")
	text, usage, err := parseTraeCLIRawChatPayload(raw)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayload error = %v", err)
	}
	if text != "OK" {
		t.Fatalf("text = %q, want OK", text)
	}
	if usage.PromptTokens != 10641 || usage.CompletionTokens != 22 || usage.TotalTokens != 10663 {
		t.Fatalf("usage = %+v, want copilot-cn token_usage", usage)
	}
}

func TestParseTraeCLIRawChatPayloadPreservesUpstreamStatusMessages(t *testing.T) {
	raw := []byte("event: output\n" +
		"data: {\"position\":20,\"message\":\"Upstream status wording may change.\",\"queue_id\":\"queue-1\"}\n\n" +
		"event: output\n" +
		"data: {\"position\":20,\"message\":\"Upstream status wording may change.\",\"queue_id\":\"queue-1\"}\n\n" +
		"event: output\n" +
		"data: {\"position\":10,\"message\":\"Updated upstream status.\",\"queue_id\":\"queue-1\"}\n\n" +
		"event: output\n" +
		"data: {\"response\":\"FINAL\",\"phase\":\"final_answer\"}\n\n" +
		"event: output\n" +
		"data: {\"response\":\"_OK\",\"phase\":null}\n\n")
	text, _, err := parseTraeCLIRawChatPayload(raw)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayload error = %v", err)
	}
	if text != "Upstream status wording may change.\nUpdated upstream status.\nFINAL_OK" {
		t.Fatalf("text = %q, want deduplicated status lines followed by FINAL_OK", text)
	}
}

func TestParseTraeCLIRawChatPayloadToolCalls(t *testing.T) {
	raw := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"README.md\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"extra_info\":{\"opaque\":\"state\"}}\n\n")
	parsed, err := parseTraeCLIRawChatPayloadDetailed(raw, nil)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayloadDetailed error = %v", err)
	}
	if parsed.Text != "" {
		t.Fatalf("text = %q, want empty", parsed.Text)
	}
	if len(parsed.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(parsed.ToolCalls))
	}
	toolCall := gjson.ParseBytes(parsed.ToolCalls[0])
	if got := toolCall.Get("id").String(); got != "call_1" {
		t.Fatalf("tool call id = %q, want call_1; raw=%s", got, parsed.ToolCalls[0])
	}
	if got := toolCall.Get("function.name").String(); got != "Read" {
		t.Fatalf("tool call name = %q, want Read; raw=%s", got, parsed.ToolCalls[0])
	}
	if got := toolCall.Get("function.arguments").String(); got != `{"path":"README.md"}` {
		t.Fatalf("tool call arguments = %q, want JSON object; raw=%s", got, parsed.ToolCalls[0])
	}
	if got := gjson.GetBytes(parsed.ExtraInfo, "opaque").String(); got != "state" {
		t.Fatalf("extra_info.opaque = %q, want state", got)
	}

	resp, err := buildTraeCLIOpenAIResponse("gpt-5.5", parsed)
	if err != nil {
		t.Fatalf("buildTraeCLIOpenAIResponse error = %v", err)
	}
	if got := gjson.GetBytes(resp, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; resp=%s", got, resp)
	}
	if got := gjson.GetBytes(resp, "choices.0.message.tool_calls.0.function.name").String(); got != "Read" {
		t.Fatalf("response tool name = %q, want Read; resp=%s", got, resp)
	}
}

func TestParseTraeCLIRawChatPayloadOutputFunctionCallToolCalls(t *testing.T) {
	raw := []byte("event: output\n" +
		"data: {\"response\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function_call\":{\"name\":\"pwd\",\"arguments\":\"\",\"partial_arguments\":null}}],\"phase\":null}\n\n" +
		"event: output\n" +
		"data: {\"response\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"\",\"function_call\":{\"name\":\"\",\"arguments\":\"{}\",\"partial_arguments\":null}}],\"phase\":null}\n\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"tool_calls\"}\n\n")
	parsed, err := parseTraeCLIRawChatPayloadDetailed(raw, nil)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayloadDetailed error = %v", err)
	}
	if parsed.Text != "" {
		t.Fatalf("text = %q, want empty", parsed.Text)
	}
	if len(parsed.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(parsed.ToolCalls))
	}
	toolCall := gjson.ParseBytes(parsed.ToolCalls[0])
	if got := toolCall.Get("id").String(); got != "call_1" {
		t.Fatalf("tool call id = %q, want call_1; raw=%s", got, parsed.ToolCalls[0])
	}
	if got := toolCall.Get("function.name").String(); got != "pwd" {
		t.Fatalf("tool call name = %q, want pwd; raw=%s", got, parsed.ToolCalls[0])
	}
	if got := toolCall.Get("function.arguments").String(); got != `{}` {
		t.Fatalf("tool call arguments = %q, want {}; raw=%s", got, parsed.ToolCalls[0])
	}
}

func TestParseTraeCLIRawChatPayloadOutputFunctionCallArgumentDeltas(t *testing.T) {
	raw := []byte("event: output\n" +
		"data: {\"response\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function_call\":{\"name\":\"Bash\",\"arguments\":\"\",\"partial_arguments\":null}}],\"phase\":null}\n\n" +
		"event: output\n" +
		"data: {\"response\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"\",\"function_call\":{\"name\":\"\",\"arguments\":\"{\\\"\",\"partial_arguments\":null}}],\"phase\":null}\n\n" +
		"event: output\n" +
		"data: {\"response\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"\",\"function_call\":{\"name\":\"\",\"arguments\":\"command\",\"partial_arguments\":null}}],\"phase\":null}\n\n" +
		"event: output\n" +
		"data: {\"response\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"\",\"function_call\":{\"name\":\"\",\"arguments\":\"\\\":\\\"pwd\\\"}\",\"partial_arguments\":null}}],\"phase\":null}\n\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"tool_calls\"}\n\n")
	parsed, err := parseTraeCLIRawChatPayloadDetailed(raw, nil)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayloadDetailed error = %v", err)
	}
	if len(parsed.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(parsed.ToolCalls))
	}
	toolCall := gjson.ParseBytes(parsed.ToolCalls[0])
	if got := toolCall.Get("function.name").String(); got != "Bash" {
		t.Fatalf("tool call name = %q, want Bash; raw=%s", got, parsed.ToolCalls[0])
	}
	if got := toolCall.Get("function.arguments").String(); got != `{"command":"pwd"}` {
		t.Fatalf("tool call arguments = %q, want full JSON; raw=%s", got, parsed.ToolCalls[0])
	}
}

func TestParseTraeCLIRawChatPayloadFunctionCall(t *testing.T) {
	raw := []byte("data: {\"functionCall\":{\"name\":\"Read\",\"args\":{\"path\":\"README.md\"}},\"extra_info\":{\"opaque\":\"state\"}}\n\n")
	parsed, err := parseTraeCLIRawChatPayloadDetailed(raw, nil)
	if err != nil {
		t.Fatalf("parseTraeCLIRawChatPayloadDetailed error = %v", err)
	}
	if len(parsed.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(parsed.ToolCalls))
	}
	toolCall := gjson.ParseBytes(parsed.ToolCalls[0])
	if got := toolCall.Get("function.name").String(); got != "Read" {
		t.Fatalf("tool call name = %q, want Read; raw=%s", got, parsed.ToolCalls[0])
	}
	if got := toolCall.Get("function.arguments").String(); got != `{"path":"README.md"}` {
		t.Fatalf("tool call arguments = %q, want JSON object; raw=%s", got, parsed.ToolCalls[0])
	}
	if got := gjson.GetBytes(parsed.ExtraInfo, "opaque").String(); got != "state" {
		t.Fatalf("extra_info.opaque = %q, want state", got)
	}
}

func TestTraeCLIExecutorExecMode(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "fake-traecli")
	capturePath := filepath.Join(tempDir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--output-last-message\" ]; then shift; out=\"$1\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"cat > " + shellQuote(capturePath) + "\n" +
		"printf 'exec says ok' > \"$out\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake traecli error = %v", err)
	}

	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		Mode:           "exec",
		ExecPath:       scriptPath,
		ExecWorkDir:    tempDir,
		BackendVariant: "max",
	}})
	resp, err := exec.Execute(context.Background(), nil, cliproxyexecutor.Request{
		Model: "gpt-5.5",
		Payload: []byte(`{
			"model":"gpt-5.5",
			"messages":[
				{"role":"system","content":"system prompt"},
				{"role":"user","content":"hello"}
			]
		}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute exec mode error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "exec says ok" {
		t.Fatalf("content = %q, want exec says ok; payload=%s", got, resp.Payload)
	}
	if got := resp.Headers.Get("X-TraeCLI-Mode"); got != "exec" {
		t.Fatalf("X-TraeCLI-Mode = %q, want exec", got)
	}
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile capture error = %v", err)
	}
	if got := string(captured); !strings.Contains(got, "system prompt") || !strings.Contains(got, "User: hello") {
		t.Fatalf("captured prompt = %q, want system and user text", got)
	}
}

func TestTraeCLIExecutorExecModeResolvesCanonicalAlias(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "fake-traecli")
	argsPath := filepath.Join(tempDir, "args.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\n" +
		"out=\"\"\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--output-last-message\" ]; then shift; out=\"$1\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"cat >/dev/null\n" +
		"printf 'exec alias ok' > \"$out\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake traecli error = %v", err)
	}

	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		Mode:           "exec",
		ExecPath:       scriptPath,
		ExecWorkDir:    tempDir,
		BackendVariant: "max",
		Models: []config.TraeCLIModel{{
			Name:      "GPT-5.4",
			Alias:     "trae-gpt-5.4",
			Aliases:   []string{"claude-haiku-4-5"},
			ModelName: "gpt-5.4__max",
		}},
	}})
	resp, err := exec.Execute(context.Background(), nil, cliproxyexecutor.Request{
		Model: "claude-haiku-4-5",
		Payload: []byte(`{
			"model":"claude-haiku-4-5",
			"messages":[{"role":"user","content":"hello"}]
		}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute exec mode error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "exec alias ok" {
		t.Fatalf("content = %q, want exec alias ok; payload=%s", got, resp.Payload)
	}
	args, errRead := os.ReadFile(argsPath)
	if errRead != nil {
		t.Fatalf("ReadFile args error = %v", errRead)
	}
	if got := string(args); !strings.Contains(got, "--model\ngpt-5.4\n") {
		t.Fatalf("exec args = %q, want canonical alias resolved to backend model gpt-5.4", got)
	}
}

func TestTraeCLIExecutorNativeFallbackToExec(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "fake-traecli")
	script := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--output-last-message\" ]; then shift; out=\"$1\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"cat >/dev/null\n" +
		"printf 'exec fallback ok' > \"$out\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake traecli error = %v", err)
	}

	var rawCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawCalls.Add(1)
		if r.URL.Path != "/api/ide/v2/llm_raw_chat" {
			t.Fatalf("raw path = %q, want llm_raw_chat", r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"RISK_CONTROL","message":"risk control blocked"}`))
	}))
	defer server.Close()

	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		Mode:                          "native",
		AuthFile:                      "testdata/trae_auth.json",
		BaseURL:                       server.URL,
		NativeFallbackToExec:          true,
		NativeFallbackCooldownSeconds: 60,
		ExecPath:                      scriptPath,
		ExecWorkDir:                   tempDir,
	}})
	req := cliproxyexecutor.Request{
		Model: "gpt-5.5",
		Payload: []byte(`{
			"model":"gpt-5.5",
			"messages":[{"role":"user","content":"hello"}]
		}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}

	resp, err := exec.Execute(context.Background(), nil, req, opts)
	if err != nil {
		t.Fatalf("Execute with native fallback error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "exec fallback ok" {
		t.Fatalf("content = %q, want exec fallback ok; payload=%s", got, resp.Payload)
	}
	if got := resp.Headers.Get("X-TraeCLI-Native-Fallback"); got != "true" {
		t.Fatalf("X-TraeCLI-Native-Fallback = %q, want true", got)
	}
	if got := rawCalls.Load(); got != 1 {
		t.Fatalf("raw calls after first request = %d, want 1", got)
	}

	if _, err := exec.Execute(context.Background(), nil, req, opts); err != nil {
		t.Fatalf("Execute during fallback cooldown error = %v", err)
	}
	if got := rawCalls.Load(); got != 1 {
		t.Fatalf("raw calls after cooldown request = %d, want still 1", got)
	}
}

func TestTraeCLIExecArgs(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		BackendVariant: "max",
		ExecSandbox:    "workspace-write",
		ExecPermission: "bypass_permissions",
		ExecExtraArgs:  []string{"--ignore-rules"},
	}})
	args := exec.execArgs(nil, "gpt-5.5", "/tmp/out.txt", "/tmp")
	got := strings.Join(args, "\x00")
	for _, want := range []string{
		"exec",
		"--model\x00gpt-5.5",
		"--config\x00model_provider='trae'",
		"--config\x00model_backend_variant='max'",
		"--output-last-message\x00/tmp/out.txt",
		"--sandbox\x00workspace-write",
		"--permission-mode\x00bypass_permissions",
		"--cd\x00/tmp",
		"--ignore-rules",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("exec args %q missing %q", args, want)
		}
	}
	if args[len(args)-1] != "-" {
		t.Fatalf("last arg = %q, want -", args[len(args)-1])
	}
}

func TestTraeCLIExecArgsUsesAvailableCacheVariant(t *testing.T) {
	modelsCache := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"models":[
		{"slug":"Kimi-K2.6","config_name":"kimi-k2.6","business_metadata":{"variants":{"standard_key":"kimi-k2.6__dev","max_key":null}}},
		{"slug":"Seed-Code","config_name":"Doubao-Seed-Code","business_metadata":{"variants":{"standard_key":"Doubao-Seed-Code__dev","max_key":"Doubao-Seed-Code__max"}}}
	]}`)
	if errWrite := os.WriteFile(modelsCache, raw, 0o600); errWrite != nil {
		t.Fatalf("write models cache: %v", errWrite)
	}
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		BackendVariant: "max",
		ModelsCache:    modelsCache,
	}})

	kimiArgs := strings.Join(exec.execArgs(nil, "kimi-k2.6", "/tmp/out.txt", "/tmp"), "\x00")
	if strings.Contains(kimiArgs, "model_backend_variant") {
		t.Fatalf("Kimi exec args should use the standard cache variant: %q", kimiArgs)
	}
	seedArgs := strings.Join(exec.execArgs(nil, "Doubao-Seed-Code", "/tmp/out.txt", "/tmp"), "\x00")
	if !strings.Contains(seedArgs, "model_backend_variant='max'") {
		t.Fatalf("Seed Code exec args should use the available max cache variant: %q", seedArgs)
	}
}

func TestTraeCLIExecArgsBypassSandboxSkipsSandboxOverrides(t *testing.T) {
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		ExecSandbox:    "read-only",
		ExecPermission: "bypass_permissions",
		ExecExtraArgs:  []string{"--dangerously-bypass-approvals-and-sandbox"},
	}})
	args := exec.execArgs(nil, "gpt-5.5", "/tmp/out.txt", "")
	got := strings.Join(args, "\x00")
	if strings.Contains(got, "--sandbox") {
		t.Fatalf("exec args = %q, want no --sandbox when bypass flag is present", args)
	}
	if strings.Contains(got, "--permission-mode") {
		t.Fatalf("exec args = %q, want no --permission-mode when bypass flag is present", args)
	}
	if !strings.Contains(got, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("exec args = %q, want bypass flag", args)
	}
}

func TestTraeCLIExecEnvPrependsGoPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		ExecEnv: map[string]string{"PATH": "/usr/local/go/bin:${PATH}", "CUSTOM": "ok"},
	}})
	env := exec.execEnv(context.Background(), nil)
	envMap := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		envMap[key] = value
	}
	if got := envMap["PATH"]; got != "/usr/local/go/bin:/usr/bin:/bin" {
		t.Fatalf("PATH = %q, want prepended Go path", got)
	}
	if got := envMap["CUSTOM"]; got != "ok" {
		t.Fatalf("CUSTOM = %q, want ok", got)
	}
}

func TestMergeTraeCLIEnv(t *testing.T) {
	got := mergeTraeCLIEnv(
		[]string{"PATH=/usr/bin", "KEEP=yes"},
		[]string{"PATH=/custom/bin", "NEW=value"},
	)
	envMap := map[string]string{}
	for _, item := range got {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		envMap[key] = value
	}
	if envMap["PATH"] != "/custom/bin" {
		t.Fatalf("PATH = %q, want shell override", envMap["PATH"])
	}
	if envMap["KEEP"] != "yes" || envMap["NEW"] != "value" {
		t.Fatalf("merged env = %#v, want KEEP and NEW", envMap)
	}
}

func TestTraeCLIClientCWDFromRequest(t *testing.T) {
	tempDir := t.TempDir()
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top level cwd",
			body: `{"cwd":` + strconv.Quote(tempDir) + `}`,
		},
		{
			name: "claude system cwd tag",
			body: `{"system":[{"type":"text","text":"<cwd>` + tempDir + `</cwd>"}]}`,
		},
		{
			name: "system message current directory",
			body: `{"messages":[{"role":"system","content":"Current directory: ` + tempDir + `"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientCWDFromRequest([]byte(tt.body)); got != tempDir {
				t.Fatalf("clientCWDFromRequest() = %q, want %q", got, tempDir)
			}
		})
	}
}

func TestTraeCLIExecWorkDirPrefersRequestCWD(t *testing.T) {
	requestDir := t.TempDir()
	configDir := t.TempDir()
	exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{ExecWorkDir: configDir}})
	body := []byte(`{"system":[{"type":"text","text":"<cwd>` + requestDir + `</cwd>"}]}`)
	if got := exec.execWorkDirForRequest(nil, body); got != requestDir {
		t.Fatalf("execWorkDirForRequest() = %q, want request cwd %q", got, requestDir)
	}
	if got := exec.execWorkDirForRequest(nil, nil); got != configDir {
		t.Fatalf("execWorkDirForRequest() fallback = %q, want config cwd %q", got, configDir)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func resetTraeCLIExtraInfoCacheForTest() {
	traeCLIExtraInfoMu.Lock()
	traeCLIExtraInfoCache = make(map[string]traeCLIExtraInfoEntry)
	traeCLIExtraInfoMu.Unlock()
}

func writeTraeCLIModelsCache(t *testing.T, baseInstructions string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models_cache.json")
	raw := []byte(`{"client_version":"0.200.17","models":[{"slug":"gpt-5.5","config_name":"gpt-5.5","base_instructions":` + strconv.Quote(baseInstructions) + `}]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile models cache: %v", err)
	}
	return path
}
