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
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	logs "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func boundaryWrite(t *testing.T, w io.Writer, value string) {
	t.Helper()
	if _, errWrite := io.WriteString(w, value); errWrite != nil {
		logs.CtxError(t.Context(), "write boundary fixture: %v", errWrite)
	}
}

func boundaryExecutor(t *testing.T, handler http.HandlerFunc) *TraeCLIExecutor {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewTraeCLIExecutor(&config.Config{TraeCLI: config.TraeCLIConfig{
		AuthFile: "testdata/trae_auth.json", BaseURL: server.URL,
		ModelsCache: writeTraeCLIModelsCache(t, ""),
		Headers:     map[string]string{"x-traecli-forward-tools": "true"},
	}})
}

func boundaryRequest(ctx context.Context, e *TraeCLIExecutor, session, prompt string, stream bool) (string, error) {
	body := fmt.Appendf(nil, `{"model":"gpt-5.5","conversation_id":%s,"messages":[{"role":"user","content":%s}],"stream":%t}`, strconv.Quote(session), strconv.Quote(prompt), stream)
	req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: body}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: body}
	if !stream {
		result, err := e.Execute(ctx, nil, req, opts)
		return string(result.Payload), err
	}
	result, err := e.ExecuteStream(ctx, nil, req, opts)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	for chunk := range result.Chunks {
		output.Write(chunk.Payload)
		if chunk.Err != nil {
			return output.String(), chunk.Err
		}
	}
	return output.String(), nil
}

func TestTraeCLICompletionBoundaries(t *testing.T) {
	const text = "data: {\"response\":\"partial\"}\n\n"
	const progress = "event: progress_notice\ndata: ;Processing_1234567890_probe\n\n"
	tests := []struct {
		name, wire string
		wantError  bool
	}{
		{"empty_eof", "", true},
		{"text_eof", text, true},
		{"tool_eof", "data: {\"tool_calls\":[{\"index\":0,\"id\":\"call_0\",\"function_call\":{\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\"}}]}\n\n", true},
		{"usage_is_not_completion", text + "event: token_usage\ndata: {\"prompt_tokens\":2,\"completion_tokens\":1}\n\n", true},
		{"state_before_eof", text + "data: {\"extra_info\":{\"opaque\":\"incomplete\"}}\n\n", true},
		{"done_marker", text + "data: [DONE]\n\n", false},
		{"done_event", text + "event: done\ndata: {}\n\n", false},
		{"finish_reason", text + "data: {\"finish_reason\":\"stop\"}\n\n", false},
		{"nested_finish", text + "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n", false},
		{"multiline_frame", "event: output\ndata: {\"response\":\ndata: \"partial\"}\n\nevent: done\ndata: {}\n\n", false},
		{"done_then_usage", text + "event: done\ndata: {\"finish_reason\":\"stop\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":2,\"completion_tokens\":1}\n\n", false},
		{"progress_notice", progress + text + "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n", false},
		{"progress_notice_eof", progress + text, true},
		{"progress_notice_is_not_completion", text + "event: progress_notice\ndata: [DONE]\n\n", true},
		{"json_progress_is_not_completion", text + "event: progress_notice\ndata: {\"finish_reason\":\"stop\",\"response\":\"Processing_probe\"}\n\n", true},
		{"progress_text_in_output_is_invalid", text + "event: output\ndata: ;Processing_1234567890_probe\n\nevent: done\ndata: {}\n\n", true},
		{"malformed_before_done", text + "data: {broken\n\ndata: [DONE]\n\n", true},
	}
	for _, tc := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
				e := boundaryExecutor(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					boundaryWrite(t, w, tc.wire)
				})
				session := uuid.NewString()
				output, err := boundaryRequest(t.Context(), e, session, "probe", stream)
				if (err != nil) != tc.wantError {
					t.Fatalf("error = %v, want error %v; output=%s", err, tc.wantError, output)
				}
				if tc.wantError && (strings.Contains(output, "response.completed") || strings.Contains(output, "response.function_call_arguments.done")) {
					t.Fatalf("incomplete stream dispatched a successful result: %s", output)
				}
				if strings.Contains(output, "Processing_") {
					t.Fatal("upstream progress marker leaked into model output")
				}
				if _, cached := getTraeCLIExtraInfo(session); tc.wantError && cached {
					t.Fatal("incomplete stream wrote session state")
				}
			})
		}
	}
}

func TestTraeCLIToolSnapshotAndDeltaBoundaries(t *testing.T) {
	top := func(id, name, args string) string {
		return fmt.Sprintf("data: {\"tool_calls\":[{\"index\":0,\"id\":%s,\"function_call\":{\"name\":%s,\"arguments\":%s}}]}\n\n", strconv.Quote(id), strconv.Quote(name), strconv.Quote(args))
	}
	snapshot := func(args string) string {
		return fmt.Sprintf("data: {\"finish_reason\":\"tool_calls\",\"tool_calls\":[{\"index\":0,\"id\":\"call_0\",\"function_call\":{\"name\":\"echo\",\"arguments\":%s}}]}\n\n", strconv.Quote(args))
	}
	explicitDelta := func(args string) string {
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_0\",\"function\":{\"name\":\"echo\",\"arguments\":%s}}]}}]}\n\n", strconv.Quote(args))
	}
	tests := []struct{ name, wire, want string }{
		{"repeated_snapshot", top("call_0", "echo", `{"value":"A"}`) + top("call_0", "echo", `{"value":"A"}`), `{"value":"A"}`},
		{"updated_explicit_snapshot", top("call_0", "echo", `{"value":"A"}`) + snapshot(`{"value":"B"}`), `{"value":"B"}`},
		{"partial_then_explicit_snapshot", top("call_0", "echo", `{"value":`) + snapshot(`{"value":"A"}`), `{"value":"A"}`},
		{"repeated_delta", top("call_0", "echo", `{"value":"`) + top("", "", "ha") + top("", "", "ha") + top("", "", `"}`), `{"value":"haha"}`},
		{"json_inside_string_delta", top("call_0", "echo", `{"value":"`) + top("", "", "{}") + top("", "", `"}`), `{"value":"{}"}`},
		{"nested_object_repeated_metadata", top("call_0", "echo", `{"outer":`) + top("call_0", "echo", `{"value":"A"}`) + top("call_0", "echo", `}`), `{"outer":{"value":"A"}}`},
		{"json_string_repeated_metadata", top("call_0", "echo", `{"value":"`) + top("call_0", "echo", `{}`) + top("call_0", "echo", `"}`), `{"value":"{}"}`},
		{"repeated_delta_with_metadata", top("call_0", "echo", `{"value":"`) + top("call_0", "echo", "ha") + top("call_0", "echo", "ha") + top("call_0", "echo", `"}`), `{"value":"haha"}`},
		{"explicit_delta_keeps_bytes", explicitDelta("{}") + explicitDelta("{}"), "{}{}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseTraeCLIRawChatPayloadDetailed([]byte(tc.wire+"data: [DONE]\n\n"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.ToolCalls) != 1 {
				t.Fatalf("tool count = %d", len(parsed.ToolCalls))
			}
			if got := gjson.GetBytes(parsed.ToolCalls[0], "function.arguments").String(); got != tc.want {
				t.Fatalf("arguments = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("custom_json_like_deltas", func(t *testing.T) {
		request := []byte(`{"tools":[{"type":"custom","name":"echo"}]}`)
		wire := top("call_0", "echo", "{}") + top("call_0", "echo", "{}") + "data: [DONE]\n\n"
		parsed, err := parseTraeCLIRawChatPayloadDetailed([]byte(wire), request)
		if err != nil {
			t.Fatal(err)
		}
		if len(parsed.ToolCalls) != 1 || gjson.GetBytes(parsed.ToolCalls[0], "function.arguments").String() != "{}{}" {
			t.Fatalf("opaque custom deltas were treated as snapshots: %s", parsed.ToolCalls)
		}
	})
}

func TestTraeCLISessionStateCompletionOrder(t *testing.T) {
	for _, oldStream := range []bool{false, true} {
		for _, newStream := range []bool{false, true} {
			for _, newFails := range []bool{false, true} {
				t.Run(fmt.Sprintf("old_stream=%v/new_stream=%v/new_fails=%v", oldStream, newStream, newFails), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					oldEntered := make(chan struct{})
					releaseOld := make(chan struct{})
					e := boundaryExecutor(t, func(w http.ResponseWriter, r *http.Request) {
						body, errRead := io.ReadAll(r.Body)
						if errRead != nil {
							logs.CtxError(r.Context(), "read boundary request: %v", errRead)
							t.Error(errRead)
							return
						}
						prompt := gjson.GetBytes(body, "messages.0.content.0.text").String()
						if prompt == "old" {
							close(oldEntered)
							select {
							case <-releaseOld:
							case <-r.Context().Done():
								return
							}
						} else if newFails {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						boundaryWrite(t, w, fmt.Sprintf("data: {\"response\":\"ok\",\"extra_info\":{\"opaque\":%s}}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n", strconv.Quote(prompt)))
					})
					session := uuid.NewString()
					oldResult := make(chan error, 1)
					go func() {
						_, err := boundaryRequest(ctx, e, session, "old", oldStream)
						oldResult <- err
					}()
					select {
					case <-oldEntered:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					_, newErr := boundaryRequest(ctx, e, session, "new", newStream)
					close(releaseOld)
					if (newErr != nil) != newFails {
						t.Fatalf("new request error = %v", newErr)
					}
					select {
					case err := <-oldResult:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					want := "new"
					if newFails {
						want = "old"
					}
					value, _ := getTraeCLIExtraInfo(session)
					if got := gjson.Get(value, "opaque").String(); got != want {
						t.Fatalf("cached state = %q, want %q", got, want)
					}
				})
			}
		}
	}
}
