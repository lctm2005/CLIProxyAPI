package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestOpenAICompatMessagesCompletionBoundaries(t *testing.T) {
	const text = "data: {\"id\":\"chatcmpl_eof\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	const finish = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	for _, tc := range []struct {
		name, wire string
		wantError  bool
	}{
		{"empty_eof", "", true},
		{"text_eof", text, true},
		{"explicit_finish_without_done", text + finish, false},
		{"done_marker", text + finish + "data: [DONE]\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				boundaryWrite(t, w, tc.wire)
			}))
			defer server.Close()
			e := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "test"}}
			body := []byte(`{"model":"fixture","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"stream":true}`)
			result, err := e.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "fixture", Payload: body}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude, OriginalRequest: body, Stream: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for chunk := range result.Chunks {
				output.Write(chunk.Payload)
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v; output=%s", err, tc.wantError, output.String())
			}
			if stopped := strings.Contains(output.String(), "message_stop"); stopped == tc.wantError {
				t.Fatalf("message_stop = %v, want %v", stopped, !tc.wantError)
			}
		})
	}
}
