package harness

import (
	"net/http"
	"testing"
)

func TestOpenAICompatMessagesEOFHTTP(t *testing.T) {
	const text = "data: {\"id\":\"chatcmpl_fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	const tool = "data: {\"id\":\"chatcmpl_fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_fixture\",\"type\":\"function\",\"function\":{\"name\":\"echo_fixture\",\"arguments\":\"{\\\"value\\\":\"}}]},\"finish_reason\":null}]}\n\n"
	const finish = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	for _, tc := range []struct {
		name, wire string
		incomplete bool
	}{
		{"empty", "", true},
		{"text", text, true},
		{"tool_arguments", tool, true},
		{"finish_without_done", text + finish, false},
		{"finish_and_done", text + finish + "data: [DONE]\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessProvider(t, "messages", "openai-compatibility")
			h.setResponder(func(w http.ResponseWriter, _ *http.Request, _ []byte, _ int) {
				writeEvents(w, []byte(tc.wire))
			})
			p := h.payload("shared-eof", true)
			h.tools(p, false)
			status, raw := h.request(p)
			result := decode("messages", raw)
			if tc.incomplete {
				h.require("incomplete-http-response", (status != 200 || result.Error) && !result.Complete, "error without successful completion", result)
				h.require("no-incomplete-tool-dispatch", len(result.Calls) == 0, 0, result.Calls)
			} else {
				h.require("compatible-completion", status == 200 && result.Complete && !result.Error && result.Text == "partial", "successful partial text fixture", result)
			}
		})
	}
}
