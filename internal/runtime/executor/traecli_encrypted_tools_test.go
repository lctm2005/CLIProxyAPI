package executor

import (
	"context"
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestTraeCLIEncryptedToolsAcrossClientFormats(t *testing.T) {
	for _, test := range []struct {
		name     string
		format   sdktranslator.Format
		property string
		toolName string
		request  func(string) string
	}{
		{
			name: "codex_additional_tools", format: sdktranslator.FormatOpenAIResponse,
			property: "message", toolName: "collaboration__spawn_agent",
			request: func(schema string) string {
				return `{"model":"gpt-5.5","input":[{"role":"user","content":"Return a tool call"},{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":` + schema + `}]}]}],"stream":true}`
			},
		},
		{
			name: "claude_messages", format: sdktranslator.FormatClaude,
			property: "content", toolName: "Write",
			request: func(schema string) string {
				return `{"model":"gpt-5.5","messages":[{"role":"user","content":"Return a tool call"}],"tools":[{"name":"Write","input_schema":` + schema + `}],"max_tokens":1024,"stream":true}`
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			contentSchema := `{"type":"object","encrypted":true,"properties":{"encrypted":{"type":"string","encrypted":true}},"required":["encrypted"],"default":{"encrypted":true}}`
			schema := `{"type":"object","properties":{"` + test.property + `":{"type":"string","encrypted":true,"contentMediaType":"application/json","contentSchema":` + contentSchema + `},"encrypted":{"type":"boolean"}},"required":["` + test.property + `","encrypted"],"additionalProperties":false,"default":{"encrypted":true}}`
			request := test.request(schema)
			original := []byte(request)
			translated := sdktranslator.TranslateRequest(test.format, sdktranslator.FormatOpenAI, "gpt-5.5", original, true)
			exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
				Headers:     map[string]string{"x-traecli-forward-tools": "true"},
				ModelsCache: writeTraeCLIModelsCache(t, ""),
			}})
			body, _, errBuild := exec.buildRawChatBodyForRequest(context.Background(), nil, cliproxyexecutor.Request{
				Model: "gpt-5.5", Payload: original,
			}, cliproxyexecutor.Options{
				SourceFormat: test.format, OriginalRequest: original,
			}, translated, original, "gpt-5.5")
			if errBuild != nil {
				t.Fatalf("buildRawChatBodyForRequest error = %v", errBuild)
			}
			if got := gjson.GetBytes(body, "tools.0.function.name").String(); got != test.toolName {
				t.Fatalf("forwarded tool name = %q, want %q", got, test.toolName)
			}
			parameters := gjson.GetBytes(body, "tools.0.function.parameters")
			if parameters.Type != gjson.String || !gjson.Valid(parameters.String()) {
				t.Fatalf("forwarded parameters must be serialized JSON: %s", parameters.Raw)
			}
			got := gjson.Parse(parameters.String())
			contentPath := "properties." + test.property + ".contentSchema"
			for _, path := range []string{"properties." + test.property + ".encrypted", contentPath + ".encrypted", contentPath + ".properties.encrypted.encrypted"} {
				if got.Get(path).Exists() {
					t.Errorf("TRAE request retained encrypted annotation at %s: %s", path, got.Raw)
				}
			}
			for path, want := range map[string]string{
				"properties." + test.property + ".type":             "string",
				"properties." + test.property + ".contentMediaType": "application/json",
				contentPath + ".type":                               "object",
				contentPath + ".properties.encrypted.type":          "string",
				contentPath + ".required.0":                         "encrypted",
				contentPath + ".default.encrypted":                  "true",
				"properties.encrypted.type":                         "boolean",
				"required.0":                                        test.property,
				"required.1":                                        "encrypted",
				"additionalProperties":                              "false",
				"default.encrypted":                                 "true",
			} {
				if value := got.Get(path); !value.Exists() || value.String() != want {
					t.Errorf("%s = %s, want %s", path, value.Raw, want)
				}
			}
			if string(original) != request {
				t.Fatal("original client request was mutated")
			}
		})
	}
}

func TestTraeCLIEncryptedToolSchema(t *testing.T) {
	const schema = `{
		"type":"object",
		"properties":{
			"task_name":{"type":"string"},
			"message":{"type":"string","encrypted":true},
			"encrypted":{"type":"boolean","encrypted":false},
			"nested":{"type":"array","items":{"type":"object","properties":{"text":{"type":"string","encrypted":true}}}},
			"choice":{"anyOf":[{"type":"string","encrypted":true},{"type":"null"}]},
			"literal":{"default":{"encrypted":true},"const":{"encrypted":false},"examples":[{"encrypted":true}]}
		},
		"$defs":{"encrypted":{"type":"string","encrypted":true}},
		"additionalProperties":false,
		"required":["task_name","message","encrypted"]
	}`
	for _, field := range []string{"parameters", "parametersJsonSchema", "input_schema"} {
		for _, serialized := range []bool{false, true} {
			t.Run(field+"/serialized="+strconv.FormatBool(serialized), func(t *testing.T) {
				value := schema
				if serialized {
					value = strconv.Quote(schema)
				}
				tool := gjson.Parse(`{"type":"function","function":{"name":"collaboration__spawn_agent","` + field + `":` + value + `}}`)
				out := normalizeTraeCLIToolForRawChat(tool)
				parameters := gjson.GetBytes(out, "function.parameters")
				if parameters.Type != gjson.String || !gjson.Valid(parameters.String()) {
					t.Fatalf("parameters must remain serialized JSON: %s", out)
				}
				got := gjson.Parse(parameters.String())
				for _, path := range []string{
					"properties.message.encrypted",
					"properties.encrypted.encrypted",
					"properties.nested.items.properties.text.encrypted",
					"properties.choice.anyOf.0.encrypted",
					"$defs.encrypted.encrypted",
				} {
					if got.Get(path).Exists() {
						t.Errorf("unsupported schema annotation survived at %s", path)
					}
				}
				for path, want := range map[string]string{
					"properties.message.type":                 "string",
					"properties.encrypted.type":               "boolean",
					"$defs.encrypted.type":                    "string",
					"required.1":                              "message",
					"required.2":                              "encrypted",
					"additionalProperties":                    "false",
					"properties.literal.default.encrypted":    "true",
					"properties.literal.const.encrypted":      "false",
					"properties.literal.examples.0.encrypted": "true",
				} {
					if value := got.Get(path); !value.Exists() || value.String() != want {
						t.Errorf("%s = %s, want %s", path, value.Raw, want)
					}
				}
				if !tool.Get("function." + field).Exists() {
					t.Fatal("input tool was mutated")
				}
			})
		}
	}
}

func TestTraeCLIToolSchemaWithoutEncryptionUnchanged(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object", "properties":{"encrypted":{"type":"boolean"}},"default":{"encrypted":true}}`,
		`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
		`not-json`,
	} {
		tool := gjson.Parse(`{"type":"function","function":{"name":"test","parameters":` + strconv.Quote(schema) + `}}`)
		out := normalizeTraeCLIToolForRawChat(tool)
		if got := gjson.GetBytes(out, "function.parameters").String(); got != schema {
			t.Errorf("schema = %s, want unchanged %s", got, schema)
		}
	}
}
