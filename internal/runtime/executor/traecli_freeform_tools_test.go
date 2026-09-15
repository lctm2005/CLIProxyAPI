package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestTraeCLIExecutorFreeformToolInputFidelity(t *testing.T) {
	const patch = "*** Begin Patch\n*** Update File: schema.js\n@@\n-SELECT \"active\" AS status;\n+SELECT 'active' AS status;\n*** End Patch\n"
	const custom = `{"type":"custom","name":"apply_patch","format":{"type":"text"}}`
	const function = `{"type":"function","name":"apply_patch","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}`
	const namespace = `{"type":"namespace","name":"editor","tools":[` + custom + `]}`
	const command = `{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}`
	tests := []struct {
		name          string
		tools         string
		additional    string
		upstreamName  string
		arguments     string
		want          string
		wantFunction  bool
		wantNamespace string
	}{
		{name: "sql_repair_patch", tools: custom, arguments: patch, want: patch},
		{name: "apostrophe", tools: custom, arguments: "*** Begin Patch\n*** Add File: note.txt\n+user's note\n*** End Patch", want: "*** Begin Patch\n*** Add File: note.txt\n+user's note\n*** End Patch"},
		{name: "whitespace_quotes_and_backslashes", tools: custom, arguments: " \t\r\n*** Begin Patch\r\n*** Add File: note.txt\r\n+用户's \"note\" C:\\new\\test\r\n*** End Patch\r\n\t ", want: " \t\r\n*** Begin Patch\r\n*** Add File: note.txt\r\n+用户's \"note\" C:\\new\\test\r\n*** End Patch\r\n\t "},
		{name: "json_like_raw_input", tools: custom, arguments: " {'status':'active'} \n", want: " {'status':'active'} \n"},
		{name: "empty_input", tools: custom},
		{name: "whitespace_only_input", tools: custom, arguments: " \t\r\n", want: " \t\r\n"},
		{name: "wrapped_input", tools: custom, arguments: `{"input":` + strconv.Quote(patch) + `}`, want: patch},
		{name: "namespaced_custom", tools: namespace, upstreamName: "editor__apply_patch", arguments: patch, want: patch, wantNamespace: "editor"},
		{name: "upstream_name_padding", tools: custom, upstreamName: " apply_patch ", arguments: patch, want: patch},
		{name: "namespace_omitted_upstream", tools: namespace, arguments: patch, want: patch, wantNamespace: "editor"},
		{name: "additional_tools", additional: namespace, upstreamName: "editor__apply_patch", arguments: patch, want: patch, wantNamespace: "editor"},
		{name: "function_wins_duplicate_declaration", tools: function, additional: custom, arguments: " {'input':'active'} ", want: `{"input":"active"}`, wantFunction: true},
		{name: "custom_wins_duplicate_declaration", tools: custom, additional: function, arguments: patch, want: patch},
		{name: "flat_function_wins_namespace_collision", tools: strings.ReplaceAll(function, `"apply_patch"`, `"editor__apply_patch"`) + "," + namespace, upstreamName: "editor__apply_patch", arguments: "{'input':'active'}", want: `{"input":"active"}`, wantFunction: true},
		{name: "ambiguous_local_name", tools: namespace + "," + strings.ReplaceAll(namespace, `"editor"`, `"other"`), arguments: "{'input':'active'}", want: `{"input":"active"}`, wantFunction: true},
		{name: "different_custom_name", tools: strings.ReplaceAll(custom, `"apply_patch"`, `"submit_patch"`), upstreamName: "submit_patch", arguments: patch, want: patch},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(tt.name+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
				name := tt.upstreamName
				if name == "" {
					name = "apply_patch"
				}
				// Mix a freeform call with a malformed ordinary function call.
				// JSON repair must remain enabled for the latter in the same response.
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					for i, call := range []struct{ name, arguments string }{{name, tt.arguments}, {"exec_command", " {'cmd':'pwd'} "}} {
						_, _ = fmt.Fprintf(w, "event: output\ndata: {\"tool_calls\":[{\"index\":%d,\"id\":\"call_%d\",\"type\":\"function\",\"function_call\":{\"name\":%s,\"arguments\":\"\"}}]}\n\n", i, i, strconv.Quote(call.name))
						runes := []rune(call.arguments)
						for _, part := range []string{string(runes[:len(runes)/2]), string(runes[len(runes)/2:])} {
							_, _ = fmt.Fprintf(w, "event: output\ndata: {\"tool_calls\":[{\"index\":%d,\"function_call\":{\"arguments\":%s}}]}\n\n", i, strconv.Quote(part))
						}
					}
					_, _ = io.WriteString(w, "event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n")
				}))
				defer server.Close()
				exec := NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
					AuthFile:    "testdata/trae_auth.json",
					BaseURL:     server.URL,
					ModelsCache: writeTraeCLIModelsCache(t, ""),
					Headers:     map[string]string{"x-traecli-forward-tools": "true"},
				}})
				tools := command
				if tt.tools != "" {
					tools += "," + tt.tools
				}
				input := `{"role":"user","content":"Return the requested tools"}`
				if tt.additional != "" {
					input += `,{"type":"additional_tools","tools":[` + tt.additional + `]}`
				}
				payload := fmt.Appendf(nil, `{"model":"gpt-5.5","input":[%s],"tools":[%s],"stream":%t}`, input, tools, stream)
				req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload}
				var response gjson.Result
				inputDoneEvents, itemDoneEvents := 0, 0
				if stream {
					result, err := exec.ExecuteStream(context.Background(), nil, req, opts)
					if err != nil {
						t.Fatalf("ExecuteStream: %v", err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("stream: %v", chunk.Err)
						}
						for _, line := range strings.Split(string(chunk.Payload), "\n") {
							if !strings.HasPrefix(line, "data:") {
								continue
							}
							event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
							if event.Get("type").String() == "response.completed" {
								response = event.Get("response")
							}
							if event.Get("type").String() == "response.custom_tool_call_input.done" && !tt.wantFunction {
								inputDoneEvents++
								if got := event.Get("input").String(); got != tt.want {
									t.Errorf("streamed input = %q, want byte-exact %q", got, tt.want)
								}
							}
							if event.Get("type").String() == "response.output_item.done" && event.Get("item.type").String() == "custom_tool_call" && !tt.wantFunction {
								itemDoneEvents++
								if got := event.Get("item.input").String(); got != tt.want {
									t.Errorf("completed item input = %q, want byte-exact %q", got, tt.want)
								}
							}
						}
					}
				} else {
					result, err := exec.Execute(context.Background(), nil, req, opts)
					if err != nil {
						t.Fatalf("Execute: %v", err)
					}
					response = gjson.ParseBytes(result.Payload)
				}
				if stream && !tt.wantFunction && (inputDoneEvents != 1 || itemDoneEvents != 1) {
					t.Errorf("custom input/item done event counts = %d/%d, want 1/1", inputDoneEvents, itemDoneEvents)
				}
				if got := response.Get("status").String(); got != "completed" {
					t.Fatalf("response status = %q, want completed; response=%s", got, response.Raw)
				}
				calls := response.Get("output").Array()
				if len(calls) != 2 {
					t.Fatalf("output count = %d, want 2; response=%s", len(calls), response.Raw)
				}
				kind, field, wantName := "custom_tool_call", "input", "apply_patch"
				if tt.wantFunction {
					kind, field = "function_call", "arguments"
				}
				if tt.wantNamespace == "" {
					wantName = strings.TrimSpace(name)
				}
				for path, want := range map[string]string{"type": kind, "name": wantName, "namespace": tt.wantNamespace, "call_id": "call_0", field: tt.want} {
					if got := calls[0].Get(path).String(); got != want {
						t.Errorf("tool %s = %q, want byte-exact %q", path, got, want)
					}
				}
				if got := calls[1].Get("type").String(); got != "function_call" {
					t.Errorf("ordinary tool type = %q, want function_call", got)
				}
				if got := calls[1].Get("arguments").String(); got != `{"cmd":"pwd"}` {
					t.Errorf("ordinary JSON repair = %q, want {\"cmd\":\"pwd\"}", got)
				}
			})
		}
	}
}
