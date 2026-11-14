package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestConfigSynthesizerTraeCLI(t *testing.T) {
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config: &config.Config{TraeCLI: config.TraeCLIConfig{
			Enabled:                       true,
			IncludeCacheModels:            true,
			Mode:                          "exec",
			Priority:                      3,
			Prefix:                        "local",
			AuthFile:                      "/tmp/trae-auth.json",
			ModelsCache:                   "/tmp/trae-models.json",
			BaseURL:                       "https://api.example.test",
			BackendVariant:                "max",
			NativeFallbackToExec:          true,
			NativeFallbackCooldownSeconds: 42,
			ExecPath:                      "/usr/local/bin/traecli",
			ExecExtraArgs:                 []string{"--ignore-rules"},
			ExecEnv:                       map[string]string{"PATH": "/usr/local/go/bin:${PATH}"},
			Models:                        []config.TraeCLIModel{{Name: "gpt-5.5", Alias: "trae-gpt"}},
		}},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auths len = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "traecli" || auth.Prefix != "local" {
		t.Fatalf("auth = %#v, want traecli/local", auth)
	}
	if got := auth.Attributes[coreauth.AttributeAPIKey]; got != "local-traecli-auth" {
		t.Fatalf("api_key attr = %q, want local marker", got)
	}
	for key, want := range map[string]string{
		"mode":                             "exec",
		"auth_file":                        "/tmp/trae-auth.json",
		"models_cache":                     "/tmp/trae-models.json",
		"base_url":                         "https://api.example.test",
		"backend_variant":                  "max",
		"include_cache_models":             "true",
		"native_fallback_to_exec":          "true",
		"native_fallback_cooldown_seconds": "42",
		"exec_path":                        "/usr/local/bin/traecli",
		"exec_extra_args":                  `["--ignore-rules"]`,
		"exec_env":                         `{"PATH":"/usr/local/go/bin:${PATH}"}`,
		"priority":                         "3",
	} {
		if got := auth.Attributes[key]; got != want {
			t.Fatalf("attr %s = %q, want %q", key, got, want)
		}
	}
	if auth.Attributes["models_hash"] == "" {
		t.Fatal("expected models_hash attr")
	}
}
