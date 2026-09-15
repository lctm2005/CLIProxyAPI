package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraeQuickstartMapsCanonicalClaudeModels(t *testing.T) {
	repoRoot, errAbs := filepath.Abs("..")
	if errAbs != nil {
		t.Fatalf("resolve repository root: %v", errAbs)
	}
	content, errRead := os.ReadFile(filepath.Join(repoRoot, "trae-quickstart.sh"))
	if errRead != nil {
		t.Fatalf("read trae-quickstart.sh: %v", errRead)
	}
	script := string(content)
	if !strings.Contains(script, "  include-cache-models: true\n") {
		t.Fatal("trae-quickstart.sh must merge supported cache models into the configured catalog")
	}
	block := "alias: \"openrouter-3o\"\n      aliases:\n        - \"claude-fable-5\"\n        - \"claude-sonnet-5\"\n        - \"claude-haiku-4-5\"\n      alias-names:\n        \"claude-sonnet-5\": \"CC Execution Route · GPT-5.5\"\n        \"claude-haiku-4-5\": \"CC Background Route · GPT-5.4\"\n      model-name: \"openrouter-3o__max\""
	if !strings.Contains(script, block) {
		t.Fatalf("trae-quickstart.sh must route every default Claude model to OpenRouter 3o with friendly names:\n%s", block)
	}
	reasoningBlock := "name: \"CC Reasoning Route · OpenRouter 3o\"\n      alias: \"claude-opus-5\"\n      model-name: \"openrouter-3o__max\""
	if !strings.Contains(script, reasoningBlock) {
		t.Fatalf("trae-quickstart.sh must expose the Claude reasoning route with a friendly name:\n%s", reasoningBlock)
	}
	for _, stale := range []string{
		"alias: \"gpt-5.6-sol\"\n      aliases:",
		"alias: \"gpt-5.5\"\n      aliases:",
		"alias: \"gpt-5.4\"\n      aliases:",
	} {
		if strings.Contains(script, stale) {
			t.Fatalf("trae-quickstart.sh retains a non-OpenRouter default Claude mapping: %s", stale)
		}
	}
}

func TestTraeQuickstartUsesBoundedErrorLogRetention(t *testing.T) {
	repoRoot, errAbs := filepath.Abs("..")
	if errAbs != nil {
		t.Fatalf("resolve repository root: %v", errAbs)
	}
	content, errRead := os.ReadFile(filepath.Join(repoRoot, "trae-quickstart.sh"))
	if errRead != nil {
		t.Fatalf("read trae-quickstart.sh: %v", errRead)
	}
	script := string(content)
	if !strings.Contains(script, "error-logs-max-files: 10\n") {
		t.Fatal("trae-quickstart.sh must retain the shared default of 10 error logs")
	}
	if strings.Contains(script, "error-logs-max-files: 10000") {
		t.Fatal("trae-quickstart.sh must not include a personal diagnostic retention override")
	}
}
