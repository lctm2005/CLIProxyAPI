package test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type codexSetupFixture struct {
	bashPath    string
	scriptPath  string
	workDir     string
	stateDir    string
	codexHome   string
	environment []string
}

type claudeSetupFixture struct {
	bashPath          string
	scriptPath        string
	workDir           string
	settingsPath      string
	gatewayModelsPath string
	curlArgsPath      string
	curlHeadPath      string
	environment       []string
}

type claudeGatewayModelsCache struct {
	BaseURL   string           `json:"baseUrl"`
	FetchedAt int64            `json:"fetchedAt"`
	Models    []map[string]any `json:"models"`
}

func newClaudeSetupFixture(t *testing.T, overrides map[string]string) *claudeSetupFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("trae-client-setup.sh requires a Unix-like environment")
	}

	bashPath, errLookPath := exec.LookPath("bash")
	if errLookPath != nil {
		t.Skip("bash is not available")
	}
	if _, errPython := exec.LookPath("python3"); errPython != nil {
		t.Skip("python3 is not available")
	}
	repoRoot, errAbs := filepath.Abs("..")
	if errAbs != nil {
		t.Fatalf("resolve repository root: %v", errAbs)
	}

	testRoot := t.TempDir()
	workDir := filepath.Join(testRoot, "work")
	stateDir := filepath.Join(testRoot, "state")
	homeDir := filepath.Join(testRoot, "home")
	binDir := filepath.Join(testRoot, "bin")
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	gatewayModelsPath := filepath.Join(homeDir, ".claude", "cache", "gateway-models.json")
	curlArgsPath := filepath.Join(testRoot, "curl-args.log")
	curlHeadPath := filepath.Join(testRoot, "curl-headers.log")
	for _, dir := range []string{workDir, stateDir, homeDir, binDir} {
		if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
			t.Fatalf("create test directory %s: %v", dir, errMkdir)
		}
	}
	if errWrite := os.WriteFile(filepath.Join(stateDir, "api-key"), []byte("test-api-key\n"), 0o600); errWrite != nil {
		t.Fatalf("write test API key: %v", errWrite)
	}

	claudePath := filepath.Join(binDir, "claude")
	writeTestExecutable(t, claudePath, "#!/usr/bin/env bash\nprintf 'claude-test 1.0\\n'\n")
	writeTestExecutable(t, filepath.Join(binDir, "curl"), `#!/usr/bin/env bash
set -euo pipefail
{
  printf 'CALL\n'
  printf '%s\n' "$@"
} >> "${TRAE_TEST_CURL_ARGS}"
cat >> "${TRAE_TEST_CURL_HEADERS}"
if [[ " $* " == *" Anthropic-Version: 2023-06-01 "* ]]; then
  if [[ -n "${TRAE_TEST_ANTHROPIC_MODELS_RESPONSE:-}" ]]; then
    printf '%s\n' "${TRAE_TEST_ANTHROPIC_MODELS_RESPONSE}"
  else
    printf '%s\n' '{"data":[{"id":"claude-fable-5-dd-o3-retuornepo","display_name":"OpenRouter 3o","description":"not cached","max_input_tokens":1000000},{"id":"claude-fable-5-dd-los-6.5-tpg","display_name":"GPT-5.6 Sol","max_input_tokens":800000},{"id":"anthropic-internal-model","display_name":"Anthropic Internal"},{"id":"claude-no-display-name"},{"id":"gpt-5.5","display_name":"GPT-5.5"}]}'
  fi
else
  printf '%s\n' '{"object":"list","data":[{"id":"openrouter-3o","object":"model"},{"id":"gpt-5.6-sol","object":"model"}]}'
fi
`)

	pathValue := binDir
	if inheritedPath := os.Getenv("PATH"); inheritedPath != "" {
		pathValue += string(os.PathListSeparator) + inheritedPath
	}
	environmentOverrides := map[string]string{
		"HOME":                      homeDir,
		"PATH":                      pathValue,
		"TRAE_CLAUDE_BACKUP_LIMIT":  "5",
		"TRAE_CLAUDE_BIN":           claudePath,
		"TRAE_CLAUDE_JSON_TOOL":     "python3",
		"TRAE_CLAUDE_SETTINGS_PATH": settingsPath,
		"TRAE_PROXY_HOST":           "127.0.0.1",
		"TRAE_PROXY_PORT":           "8317",
		"TRAE_PROXY_STATE_DIR":      stateDir,
		"TRAE_TEST_CURL_ARGS":       curlArgsPath,
		"TRAE_TEST_CURL_HEADERS":    curlHeadPath,
	}
	for key, value := range overrides {
		environmentOverrides[key] = value
	}

	return &claudeSetupFixture{
		bashPath:          bashPath,
		scriptPath:        filepath.Join(repoRoot, "trae-client-setup.sh"),
		workDir:           workDir,
		settingsPath:      settingsPath,
		gatewayModelsPath: gatewayModelsPath,
		curlArgsPath:      curlArgsPath,
		curlHeadPath:      curlHeadPath,
		environment:       envWithOverrides(environmentOverrides),
	}
}

func (fixture *claudeSetupFixture) run(t *testing.T) string {
	t.Helper()
	command := exec.Command(fixture.bashPath, fixture.scriptPath, "claude")
	command.Dir = fixture.workDir
	command.Env = fixture.environment
	output, errRun := command.CombinedOutput()
	if errRun != nil {
		t.Fatalf("Claude setup failed: %v\n%s", errRun, output)
	}
	return string(output)
}

func (fixture *claudeSetupFixture) readSettings(t *testing.T) struct {
	Model          string            `json:"model"`
	ModelOverrides map[string]string `json:"modelOverrides"`
	Env            map[string]string `json:"env"`
} {
	t.Helper()
	raw, errRead := os.ReadFile(fixture.settingsPath)
	if errRead != nil {
		t.Fatalf("read Claude settings: %v", errRead)
	}
	var settings struct {
		Model          string            `json:"model"`
		ModelOverrides map[string]string `json:"modelOverrides"`
		Env            map[string]string `json:"env"`
	}
	if errUnmarshal := json.Unmarshal(raw, &settings); errUnmarshal != nil {
		t.Fatalf("parse Claude settings: %v\n%s", errUnmarshal, raw)
	}
	return settings
}

func assertClaudeModelOverrides(t *testing.T, got map[string]string) {
	t.Helper()
	want := map[string]string{
		"claude-fable-5":   "gpt-5.6-sol",
		"claude-opus-5":    "openrouter-3o",
		"claude-sonnet-5":  "gpt-5.5",
		"claude-haiku-4-5": "gpt-5.4",
	}
	for canonicalModel, wantProviderModel := range want {
		if gotProviderModel := got[canonicalModel]; gotProviderModel != wantProviderModel {
			t.Fatalf("modelOverrides[%q] = %q, want %q", canonicalModel, gotProviderModel, wantProviderModel)
		}
	}
}

func (fixture *claudeSetupFixture) readGatewayModels(t *testing.T) claudeGatewayModelsCache {
	t.Helper()
	raw, errRead := os.ReadFile(fixture.gatewayModelsPath)
	if errRead != nil {
		t.Fatalf("read Claude gateway model cache: %v", errRead)
	}
	var cache claudeGatewayModelsCache
	if errUnmarshal := json.Unmarshal(raw, &cache); errUnmarshal != nil {
		t.Fatalf("parse Claude gateway model cache: %v\n%s", errUnmarshal, raw)
	}
	return cache
}

func newCodexSetupFixture(t *testing.T, relativeStateDir bool) *codexSetupFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("trae-client-setup.sh requires a Unix-like environment")
	}

	bashPath, errLookPath := exec.LookPath("bash")
	if errLookPath != nil {
		t.Skip("bash is not available")
	}
	repoRoot, errAbs := filepath.Abs("..")
	if errAbs != nil {
		t.Fatalf("resolve repository root: %v", errAbs)
	}

	testRoot := t.TempDir()
	workDir := filepath.Join(testRoot, "work")
	stateDir := filepath.Join(testRoot, "state")
	stateDirEnv := stateDir
	if relativeStateDir {
		stateDir = filepath.Join(workDir, "state")
		stateDirEnv = "state"
	}
	codexHome := filepath.Join(testRoot, "codex")
	binDir := filepath.Join(testRoot, "bin")
	for _, dir := range []string{workDir, stateDir, codexHome, binDir, filepath.Join(testRoot, "home")} {
		if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
			t.Fatalf("create test directory %s: %v", dir, errMkdir)
		}
	}
	if errWrite := os.WriteFile(filepath.Join(stateDir, "api-key"), []byte("test-api-key\n"), 0o600); errWrite != nil {
		t.Fatalf("write test API key: %v", errWrite)
	}

	codexPath := filepath.Join(binDir, "codex")
	writeTestExecutable(t, codexPath, "#!/usr/bin/env bash\nprintf 'codex-test 1.0\\n'\n")
	writeTestExecutable(t, filepath.Join(binDir, "curl"), "#!/usr/bin/env bash\nexit 1\n")

	pathValue := binDir
	if inheritedPath := os.Getenv("PATH"); inheritedPath != "" {
		pathValue += string(os.PathListSeparator) + inheritedPath
	}
	environment := envWithOverrides(map[string]string{
		"CODEX_HOME":              codexHome,
		"HOME":                    filepath.Join(testRoot, "home"),
		"PATH":                    pathValue,
		"TRAE_CODEX_BACKUP_LIMIT": "5",
		"TRAE_CODEX_BIN":          codexPath,
		"TRAE_CODEX_MODEL":        "gpt-5.5",
		"TRAE_PROXY_HOST":         "127.0.0.1",
		"TRAE_PROXY_PORT":         "1",
		"TRAE_PROXY_STATE_DIR":    stateDirEnv,
	})

	return &codexSetupFixture{
		bashPath:    bashPath,
		scriptPath:  filepath.Join(repoRoot, "trae-client-setup.sh"),
		workDir:     workDir,
		stateDir:    stateDir,
		codexHome:   codexHome,
		environment: environment,
	}
}

func writeTestExecutable(t *testing.T, path string, content string) {
	t.Helper()
	if errWrite := os.WriteFile(path, []byte(content), 0o700); errWrite != nil {
		t.Fatalf("write test executable %s: %v", path, errWrite)
	}
}

func envWithOverrides(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; !overridden {
			environment = append(environment, entry)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		environment = append(environment, key+"="+overrides[key])
	}
	return environment
}

func (fixture *codexSetupFixture) run(t *testing.T) (string, error) {
	t.Helper()
	command := exec.Command(fixture.bashPath, fixture.scriptPath, "codex", "--allow-offline")
	command.Dir = fixture.workDir
	command.Env = fixture.environment
	output, errRun := command.CombinedOutput()
	return string(output), errRun
}

func TestTraeClientSetupClaudeDefaultsUseConfiguredModelFamiliesWithoutPinningMainModel(t *testing.T) {
	fixture := newClaudeSetupFixture(t, nil)
	fixture.run(t)
	settings := fixture.readSettings(t)

	if settings.Model != "openrouter-3o" {
		t.Fatalf("main model = %q, want openrouter-3o", settings.Model)
	}
	if got, exists := settings.Env["ANTHROPIC_MODEL"]; exists {
		t.Fatalf("ANTHROPIC_MODEL = %q, want it unset so Claude Code can select the main model", got)
	}
	wantModels := map[string]string{
		"ANTHROPIC_DEFAULT_FABLE_MODEL":  "gpt-5.6-sol",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "openrouter-3o",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "gpt-5.5",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "gpt-5.4",
	}
	for key, want := range wantModels {
		if got := settings.Env[key]; got != want {
			t.Fatalf("%s = %q, want %s", key, got, want)
		}
	}
	wantNames := map[string]string{
		"ANTHROPIC_DEFAULT_FABLE_MODEL_NAME":  "GPT-5.6 Sol",
		"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME":   "OpenRouter 3o",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "GPT-5.5",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME":  "GPT-5.4",
	}
	for key, want := range wantNames {
		if got := settings.Env[key]; got != want {
			t.Fatalf("%s = %q, want %s", key, got, want)
		}
	}
	if got := settings.Env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"]; got != "1000000" {
		t.Fatalf("CLAUDE_CODE_MAX_CONTEXT_TOKENS = %q, want 1000000", got)
	}
	assertClaudeModelOverrides(t, settings.ModelOverrides)

	args, errReadArgs := os.ReadFile(fixture.curlArgsPath)
	if errReadArgs != nil {
		t.Fatalf("read curl args: %v", errReadArgs)
	}
	if strings.Contains(string(args), "test-api-key") {
		t.Fatalf("proxy API key leaked into curl argv: %s", args)
	}
	headers, errReadHeaders := os.ReadFile(fixture.curlHeadPath)
	if errReadHeaders != nil {
		t.Fatalf("read curl headers: %v", errReadHeaders)
	}
	if !strings.Contains(string(headers), "Authorization: Bearer test-api-key") {
		t.Fatalf("curl did not receive the proxy credential through stdin: %s", headers)
	}
}

func TestTraeClientSetupClaudePrewarmsGatewayModels(t *testing.T) {
	for _, jsonTool := range []string{"jq", "node", "python3"} {
		t.Run(jsonTool, func(t *testing.T) {
			if _, errLookPath := exec.LookPath(jsonTool); errLookPath != nil {
				t.Skipf("%s is not available", jsonTool)
			}
			fixture := newClaudeSetupFixture(t, map[string]string{
				"TRAE_CLAUDE_JSON_TOOL": jsonTool,
			})
			output := fixture.run(t)
			settings := fixture.readSettings(t)
			cache := fixture.readGatewayModels(t)
			assertClaudeModelOverrides(t, settings.ModelOverrides)

			if cache.BaseURL != "http://127.0.0.1:8317" {
				t.Fatalf("cache baseUrl = %q, want local proxy", cache.BaseURL)
			}
			if cache.FetchedAt <= 0 {
				t.Fatalf("cache fetchedAt = %d, want a positive Unix timestamp", cache.FetchedAt)
			}
			wantIDs := []string{
				"claude-fable-5-dd-o3-retuornepo",
				"claude-fable-5-dd-los-6.5-tpg",
				"anthropic-internal-model",
				"claude-no-display-name",
			}
			if len(cache.Models) != len(wantIDs) {
				t.Fatalf("cached model count = %d, want %d: %#v", len(cache.Models), len(wantIDs), cache.Models)
			}
			for index, wantID := range wantIDs {
				model := cache.Models[index]
				if got, _ := model["id"].(string); got != wantID {
					t.Fatalf("cached model %d id = %q, want %q", index, got, wantID)
				}
				for key := range model {
					if key != "id" && key != "display_name" {
						t.Fatalf("cached model %q retained unsupported field %q", wantID, key)
					}
				}
			}
			cacheInfo, errStat := os.Stat(fixture.gatewayModelsPath)
			if errStat != nil {
				t.Fatalf("stat Claude gateway model cache: %v", errStat)
			}
			if got := cacheInfo.Mode().Perm(); got != 0o600 {
				t.Fatalf("gateway model cache mode = %o, want 600", got)
			}
			if !strings.Contains(output, "Prewarmed Claude Code model cache with 4 models") {
				t.Fatalf("setup output did not report prewarmed models:\n%s", output)
			}

			args, errReadArgs := os.ReadFile(fixture.curlArgsPath)
			if errReadArgs != nil {
				t.Fatalf("read curl args: %v", errReadArgs)
			}
			for _, want := range []string{
				"Anthropic-Version: 2023-06-01",
				"User-Agent: claude-cli/",
				"/v1/models?limit=1000",
			} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("prewarm curl args do not contain %q:\n%s", want, args)
				}
			}
		})
	}
}

func TestTraeClientSetupClaudeKeepsExistingGatewayModelsWhenPrewarmHasNoUsableModels(t *testing.T) {
	fixture := newClaudeSetupFixture(t, map[string]string{
		"TRAE_TEST_ANTHROPIC_MODELS_RESPONSE": `{"data":[{"id":"gpt-5.5","display_name":"GPT-5.5"}]}`,
	})
	if errMkdir := os.MkdirAll(filepath.Dir(fixture.gatewayModelsPath), 0o700); errMkdir != nil {
		t.Fatalf("create Claude cache directory: %v", errMkdir)
	}
	existing := []byte("{\"baseUrl\":\"https://old.example\",\"fetchedAt\":1,\"models\":[{\"id\":\"claude-old\"}]}\n")
	if errWrite := os.WriteFile(fixture.gatewayModelsPath, existing, 0o600); errWrite != nil {
		t.Fatalf("write existing Claude gateway model cache: %v", errWrite)
	}

	output := fixture.run(t)
	raw, errRead := os.ReadFile(fixture.gatewayModelsPath)
	if errRead != nil {
		t.Fatalf("read preserved Claude gateway model cache: %v", errRead)
	}
	if string(raw) != string(existing) {
		t.Fatalf("failed prewarm replaced existing cache:\ngot:  %s\nwant: %s", raw, existing)
	}
	if !strings.Contains(output, "Could not prewarm Claude Code model cache") {
		t.Fatalf("setup output did not warn about failed prewarm:\n%s", output)
	}
}

func TestTraeClientSetupClaudePreservesUnmanagedModelOverrides(t *testing.T) {
	fixture := newClaudeSetupFixture(t, nil)
	if errMkdir := os.MkdirAll(filepath.Dir(fixture.settingsPath), 0o700); errMkdir != nil {
		t.Fatalf("create Claude settings directory: %v", errMkdir)
	}
	existing := []byte(`{"modelOverrides":{"claude-custom":"provider-custom","claude-fable-5":"stale-model"}}`)
	if errWrite := os.WriteFile(fixture.settingsPath, existing, 0o600); errWrite != nil {
		t.Fatalf("write existing Claude settings: %v", errWrite)
	}

	fixture.run(t)
	settings := fixture.readSettings(t)
	assertClaudeModelOverrides(t, settings.ModelOverrides)
	if got := settings.ModelOverrides["claude-custom"]; got != "provider-custom" {
		t.Fatalf("unmanaged model override = %q, want provider-custom", got)
	}
}

func TestTraeClientSetupClaudeResolvesOverriddenMainModelByID(t *testing.T) {
	fixture := newClaudeSetupFixture(t, map[string]string{
		"TRAE_CLAUDE_MODEL": "gpt-5.6-sol",
	})
	fixture.run(t)
	settings := fixture.readSettings(t)

	if settings.Model != "gpt-5.6-sol" {
		t.Fatalf("main model = %q, want gpt-5.6-sol", settings.Model)
	}
	if got := settings.Env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"]; got != "800000" {
		t.Fatalf("CLAUDE_CODE_MAX_CONTEXT_TOKENS = %q, want overridden model window 800000", got)
	}
}

func TestTraeClientSetupClaudeWithoutPython(t *testing.T) {
	for _, jsonTool := range []string{"jq", "node"} {
		t.Run(jsonTool, func(t *testing.T) {
			fixture := newClaudeSetupFixture(t, map[string]string{"TRAE_CLAUDE_JSON_TOOL": jsonTool})
			binDir := filepath.Join(filepath.Dir(fixture.workDir), "bin")
			// Only expose the selected JSON processor and the shell utilities.
			for _, tool := range []string{jsonTool, "bash", "cat", "chmod", "date", "dirname", "head", "mkdir", "mktemp", "mv", "tr", "rm", "cp", "cmp"} {
				path, errLookPath := exec.LookPath(tool)
				if errLookPath != nil {
					t.Skipf("%s is not available", tool)
				}
				if errLink := os.Symlink(path, filepath.Join(binDir, tool)); errLink != nil {
					t.Fatal(errLink)
				}
			}
			for i, value := range fixture.environment {
				if strings.HasPrefix(value, "PATH=") {
					fixture.environment[i] = "PATH=" + binDir
				}
			}
			output := fixture.run(t)
			settings := fixture.readSettings(t)
			if settings.Model != "openrouter-3o" || settings.Env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "" {
				t.Fatalf("unexpected settings without Python: %#v", settings)
			}
			if !strings.Contains(output, "python3 unavailable") || len(fixture.readGatewayModels(t).Models) == 0 {
				t.Fatalf("expected context warning and model cache prewarm: %s", output)
			}
		})
	}
}

func TestTraeClientSetupClaudeResolvesEffectiveMainModel(t *testing.T) {
	for _, jsonTool := range []string{"jq", "node", "python3"} {
		if _, errLookPath := exec.LookPath(jsonTool); errLookPath != nil {
			continue
		}
		for _, tc := range []struct {
			name, saved, explicit, overrides, wantModel, wantContext string
		}{
			{name: "saved public ID", saved: "gpt-5.6-sol", wantModel: "gpt-5.6-sol", wantContext: "800000"},
			{name: "saved Claude ID", saved: "claude-fable-5-dd-los-6.5-tpg", wantModel: "claude-fable-5-dd-los-6.5-tpg", wantContext: "800000"},
			{name: "explicit Claude ID", saved: "openrouter-3o", explicit: "claude-fable-5-dd-los-6.5-tpg", wantModel: "claude-fable-5-dd-los-6.5-tpg", wantContext: "800000"},
			{name: "unknown saved ID", saved: "unavailable-model", wantModel: "unavailable-model"},
			{name: "unknown explicit ID", saved: "openrouter-3o", explicit: "unavailable-model", wantModel: "unavailable-model"},
			{name: "saved mapped ID", saved: "custom-model", overrides: `{"custom-model":"gpt-5.6-sol"}`, wantModel: "custom-model", wantContext: "800000"},
			{name: "saved fable slot", saved: "fable", wantModel: "fable", wantContext: "800000"},
		} {
			t.Run(jsonTool+"/"+tc.name, func(t *testing.T) {
				fixture := newClaudeSetupFixture(t, map[string]string{
					"TRAE_CLAUDE_JSON_TOOL": jsonTool,
					"TRAE_CLAUDE_MODEL":     tc.explicit,
				})
				if errMkdir := os.MkdirAll(filepath.Dir(fixture.settingsPath), 0o700); errMkdir != nil {
					t.Fatal(errMkdir)
				}
				settings := map[string]any{"model": tc.saved, "env": map[string]string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS": "123"}}
				if tc.overrides != "" {
					var overrides map[string]string
					if errJSON := json.Unmarshal([]byte(tc.overrides), &overrides); errJSON != nil {
						t.Fatal(errJSON)
					}
					settings["modelOverrides"] = overrides
				}
				raw, errJSON := json.Marshal(settings)
				if errJSON != nil {
					t.Fatal(errJSON)
				}
				if errWrite := os.WriteFile(fixture.settingsPath, raw, 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
				output := fixture.run(t)
				got := fixture.readSettings(t)
				if got.Model != tc.wantModel || got.Env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != tc.wantContext {
					t.Fatalf("model/context = %q/%q, want %q/%q", got.Model, got.Env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"], tc.wantModel, tc.wantContext)
				}
				if !strings.Contains(output, "Main model: "+tc.wantModel+"\n") {
					t.Fatalf("setup did not report the effective model: %s", output)
				}
			})
		}
	}
}

func TestTraeClientSetupCodexReplacesQuotedRootModelKeys(t *testing.T) {
	fixture := newCodexSetupFixture(t, false)
	configPath := filepath.Join(fixture.codexHome, "config.toml")
	input := "\"model\" = \"old-model\"\n'model_provider' = 'openai'\npersonality = \"pragmatic\"\n"
	if errWrite := os.WriteFile(configPath, []byte(input), 0o600); errWrite != nil {
		t.Fatalf("write initial Codex config: %v", errWrite)
	}

	output, errRun := fixture.run(t)
	if errRun != nil {
		t.Fatalf("setup failed: %v\n%s", errRun, output)
	}
	config, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("read generated Codex config: %v", errRead)
	}
	got := string(config)
	if !strings.HasPrefix(got, "model = \"gpt-5.5\"\nmodel_provider = \"trae_cli_proxy\"\n") {
		t.Fatalf("generated config has unexpected model keys:\n%s", got)
	}
	for _, staleKey := range []string{"\"model\" = \"old-model\"", "'model_provider' = 'openai'"} {
		if strings.Contains(got, staleKey) {
			t.Fatalf("generated config retained stale key %q:\n%s", staleKey, got)
		}
	}
	if !strings.Contains(got, "personality = \"pragmatic\"") {
		t.Fatalf("generated config did not preserve unrelated settings:\n%s", got)
	}
}

func TestTraeClientSetupCodexRejectsInlineProviderConflict(t *testing.T) {
	fixture := newCodexSetupFixture(t, false)
	configPath := filepath.Join(fixture.codexHome, "config.toml")
	input := "[model_providers]\ntrae_cli_proxy = { name = \"Existing\", base_url = \"https://example.test/v1\", wire_api = \"responses\" }\n"
	if errWrite := os.WriteFile(configPath, []byte(input), 0o600); errWrite != nil {
		t.Fatalf("write initial Codex config: %v", errWrite)
	}

	output, errRun := fixture.run(t)
	if errRun == nil {
		t.Fatalf("setup unexpectedly accepted a conflicting inline provider:\n%s", output)
	}
	if !strings.Contains(output, "already exists outside the TRAE managed block") {
		t.Fatalf("setup returned an unexpected error:\n%s", output)
	}
	config, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("read Codex config after failure: %v", errRead)
	}
	if string(config) != input {
		t.Fatalf("setup changed the config after rejecting it:\n%s", config)
	}
}

func TestTraeClientSetupCodexHelperUsesAbsoluteStateDir(t *testing.T) {
	fixture := newCodexSetupFixture(t, true)
	output, errRun := fixture.run(t)
	if errRun != nil {
		t.Fatalf("setup failed: %v\n%s", errRun, output)
	}

	otherDir := t.TempDir()
	helperPath := filepath.Join(fixture.stateDir, "codex-api-key-helper.sh")
	command := exec.Command(helperPath)
	command.Dir = otherDir
	command.Env = fixture.environment
	helperOutput, errHelper := command.CombinedOutput()
	if errHelper != nil {
		t.Fatalf("API key helper failed from another directory: %v\n%s", errHelper, helperOutput)
	}
	if got := string(helperOutput); got != "test-api-key\n" {
		t.Fatalf("API key helper output = %q, want %q", got, "test-api-key\n")
	}
}
