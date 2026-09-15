package responses

import (
	"context"
	"fmt"
	"strconv"
	"testing"
)

func TestCustomToolEmptyInputCompletionEvents(t *testing.T) {
	for _, tc := range []struct{ name, kind, arguments, want string }{
		{"raw_empty", "custom", "", ""},
		{"wrapped_empty", "custom", `{"input":""}`, ""},
		{"whitespace", "custom", " \t\n", " \t\n"},
		{"ordinary_function", "function", "", "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := fmt.Appendf(nil, `{"model":"fixture","tools":[{"type":%s,"name":"exec"}]}`, strconv.Quote(tc.kind))
			chunks := []string{
				fmt.Sprintf(`data: {"id":"chatcmpl_empty","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_empty","type":"function","function":{"name":"exec","arguments":%s}}]},"finish_reason":null}]}`, strconv.Quote(tc.arguments)),
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				"data: [DONE]",
			}
			var state any
			seen := 0
			for _, chunk := range chunks {
				for _, output := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "fixture", request, nil, []byte(chunk), &state) {
					event, data := parseOpenAIResponsesSSEEvent(t, output)
					field := "input"
					if tc.kind == "function" {
						field = "arguments"
					}
					path := ""
					switch event {
					case "response.custom_tool_call_input.done":
						path = "input"
					case "response.function_call_arguments.done":
						path = "arguments"
					case "response.output_item.done":
						path = "item." + field
					case "response.completed":
						if tc.kind == "function" {
							continue // This test protects the existing function item defaults.
						}
						path = "response.output.0." + field
					}
					if path != "" {
						seen++
						if value := data.Get(path); !value.Exists() || value.String() != tc.want {
							t.Errorf("%s: input = %q, want %q", event, value.String(), tc.want)
						}
					}
				}
			}
			wantEvents := 3
			if tc.kind == "function" {
				wantEvents = 2
			}
			if seen != wantEvents {
				t.Fatalf("completion events = %d, want %d", seen, wantEvents)
			}
		})
	}
}
