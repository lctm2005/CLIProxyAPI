// Package harness drives the real HTTP router, auth manager, TRAE executor and translators.
package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	configaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	logs "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const model = "gpt-5.5"

type assertion struct {
	ID       string `json:"id"`
	Pass     bool   `json:"pass"`
	Expected any    `json:"expected"`
	Observed any    `json:"observed"`
}

type harness struct {
	t           *testing.T
	protocol    string
	root        string
	art         string
	cfg         *config.Config
	manager     *auth.Manager
	proxy       *httptest.Server
	upstream    *httptest.Server
	client      *http.Client
	mu          sync.Mutex
	respond     func(http.ResponseWriter, *http.Request, []byte, int)
	requests    [][]byte
	assertions  []assertion
	active      atomic.Int64
	frontActive atomic.Int64
	requestSeq  atomic.Int64
	clientID    string
	credential  *auth.Auth
}

func checkErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		logs.CtxError(context.Background(), "TRAE harness: %v", err)
		t.Fatal(err)
	}
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		logs.CtxError(context.Background(), "encode fixture: %v", err)
		panic(err)
	}
	return b
}

func newHarness(t *testing.T, protocol string) *harness {
	t.Helper()
	return newHarnessProvider(t, protocol, "traecli")
}

func newHarnessProvider(t *testing.T, protocol, provider string) *harness {
	t.Helper()
	h := &harness{t: t, protocol: protocol, root: t.TempDir(), art: os.Getenv("TRAE_TEST_ARTIFACT_DIR"), clientID: uuid.NewString()}
	if h.art == "" {
		h.art = filepath.Join(h.root, "artifacts")
	}
	checkErr(t, os.MkdirAll(h.art, 0o700))
	h.respond = func(w http.ResponseWriter, r *http.Request, body []byte, n int) {
		writeEvents(w, textEvent("fixture-ok"), doneEvent())
	}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.active.Add(1)
		defer h.active.Add(-1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			logs.CtxError(r.Context(), "read fake upstream request: %v", err)
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, bytes.Clone(body))
		n := len(h.requests)
		handler := h.respond
		h.mu.Unlock()
		if errWrite := os.WriteFile(filepath.Join(h.art, fmt.Sprintf("upstream-%04d.request.json", n)), body, 0o600); errWrite != nil {
			logs.CtxError(r.Context(), "save upstream request: %v", errWrite)
			t.Error(errWrite)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		handler(w, r, body, n)
	}))
	authPath := filepath.Join(h.root, "auth.json")
	checkErr(t, os.WriteFile(authPath, []byte(`{"trae":{"access_token":"fixture-trae-token","user_id":"fixture-user","expires_at":"2099-01-01T00:00:00Z"}}`), 0o600))
	cachePath := filepath.Join(h.root, "models.json")
	checkErr(t, os.WriteFile(cachePath, []byte(`{"models":[{"slug":"gpt-5.5","config_name":"gpt-5.5","base_instructions":"fixture-base","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`), 0o600))
	h.cfg = &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"fixture-proxy-key"}}, AuthDir: filepath.Join(h.root, "auths"), Host: "127.0.0.1", CommercialMode: true, DisableCooling: true, RequestRetry: 0,
		TraeCLI: config.TraeCLIConfig{Enabled: true, Mode: "native", BaseURL: h.upstream.URL, AuthFile: authPath, ModelsCache: cachePath, BackendVariant: "max", Headers: map[string]string{"x-traecli-forward-tools": "true"}, Models: []config.TraeCLIModel{{Name: model, Alias: model, ModelName: model + "__max"}}}}
	checkErr(t, os.MkdirAll(h.cfg.AuthDir, 0o700))
	h.manager = auth.NewManager(nil, nil, nil)
	h.manager.SetConfig(h.cfg)
	configaccess.Register(&h.cfg.SDKConfig)
	attributes := map[string]string{"base_url": h.upstream.URL, "auth_file": authPath}
	switch provider {
	case "traecli":
		h.manager.RegisterExecutor(executor.NewTraeCLIExecutor(h.cfg))
	case "openai-compatibility":
		h.cfg.TraeCLI.Enabled = false
		h.manager.RegisterExecutor(executor.NewOpenAICompatExecutor(provider, h.cfg))
		attributes = map[string]string{"base_url": h.upstream.URL + "/v1", "api_key": "fixture-upstream-key"}
	default:
		t.Fatalf("unsupported fixture provider %q", provider)
	}
	h.credential = &auth.Auth{ID: h.clientID, Provider: provider, Status: auth.StatusActive, Attributes: attributes}
	_, err := h.manager.Register(t.Context(), h.credential)
	checkErr(t, err)
	registry.GetGlobalRegistry().RegisterClient(h.clientID, provider, []*registry.ModelInfo{{ID: model, Object: "model", OwnedBy: provider, Name: model}})
	var engine *gin.Engine
	api.NewServer(h.cfg, h.manager, access.NewManager(), filepath.Join(h.root, "config.yaml"), api.WithEngineConfigurator(func(e *gin.Engine) { engine = e }), api.WithMiddleware(func(c *gin.Context) { h.frontActive.Add(1); defer h.frontActive.Add(-1); c.Next() }))
	h.proxy = httptest.NewServer(engine)
	h.client = &http.Client{Transport: &http.Transport{MaxIdleConns: 16, MaxIdleConnsPerHost: 8}}
	t.Cleanup(func() {
		h.client.CloseIdleConnections()
		h.proxy.Close()
		h.upstream.Close()
		registry.GetGlobalRegistry().UnregisterClient(h.clientID)
		result := map[string]any{"case_id": os.Getenv("TRAE_TEST_CASE"), "protocol": protocol, "variant": os.Getenv("TRAE_TEST_VARIANT"), "assertions": h.assertions, "upstream_requests": len(h.requests), "verdict": "PASS"}
		if t.Failed() {
			result["verdict"] = "FAIL"
		}
		checkErr(t, os.WriteFile(filepath.Join(h.art, "result.json"), encode(result), 0o600))
	})
	return h
}

func (h *harness) require(id string, passed bool, expected, observed any) {
	h.t.Helper()
	h.mu.Lock()
	h.assertions = append(h.assertions, assertion{ID: id, Pass: passed, Expected: expected, Observed: observed})
	h.mu.Unlock()
	if !passed {
		h.t.Errorf("%s: expected %v; observed %v", id, expected, observed)
	}
}

func (h *harness) setResponder(fn func(http.ResponseWriter, *http.Request, []byte, int)) {
	h.mu.Lock()
	h.respond = fn
	h.mu.Unlock()
}
func (h *harness) count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.requests) }
func (h *harness) captured() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.requests...)
}

func (h *harness) payload(prompt string, stream bool) map[string]any {
	p := map[string]any{"model": model, "stream": stream}
	switch h.protocol {
	case "responses":
		p["input"] = []any{map[string]any{"role": "user", "content": prompt}}
		p["max_output_tokens"] = 4096
	case "messages":
		p["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
		p["max_tokens"] = 4096
	default:
		p["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
		p["max_tokens"] = 4096
	}
	return p
}

func (h *harness) endpoint() string {
	return map[string]string{"responses": "/v1/responses", "messages": "/v1/messages", "chat_completions": "/v1/chat/completions"}[h.protocol]
}

func (h *harness) open(ctx context.Context, payload map[string]any, key string) (*http.Response, error) {
	raw := encode(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.proxy.URL+h.endpoint(), bytes.NewReader(raw))
	if err != nil {
		logs.CtxError(ctx, "create downstream request: %v", err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.protocol == "messages" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		logs.CtxError(ctx, "send downstream request: %v", err)
	}
	return resp, err
}

func (h *harness) request(payload map[string]any) (int, []byte) {
	h.t.Helper()
	sequence := h.requestSeq.Add(1)
	checkErr(h.t, os.WriteFile(filepath.Join(h.art, fmt.Sprintf("downstream-%04d.request.json", sequence)), encode(payload), 0o600))
	resp, err := h.open(h.t.Context(), payload, "fixture-proxy-key")
	checkErr(h.t, err)
	raw, err := io.ReadAll(resp.Body)
	checkErr(h.t, err)
	checkErr(h.t, resp.Body.Close())
	checkErr(h.t, os.WriteFile(filepath.Join(h.art, fmt.Sprintf("downstream-%04d.response.bin", sequence)), raw, 0o600))
	if resp.StatusCode == 200 && h.credential.Provider == "traecli" {
		h.require("native-mode", resp.Header.Get("X-TraeCLI-Mode") == "native", "native", resp.Header.Get("X-TraeCLI-Mode"))
		h.require("no-fallback", resp.Header.Get("X-TraeCLI-Native-Fallback") != "true", false, resp.Header.Get("X-TraeCLI-Native-Fallback"))
	}
	return resp.StatusCode, raw
}

func textEvent(text string) []byte {
	return append(append([]byte("event: output\ndata: "), encode(map[string]any{"response": text})...), []byte("\n\n")...)
}
func doneEvent() []byte { return []byte("event: done\ndata: {\"finish_reason\":\"stop\"}\n\n") }
func toolEvent(id, name, input, kind string) []byte {
	return append(append([]byte("event: output\ndata: "), encode(map[string]any{"response": "", "tool_calls": []any{map[string]any{"index": 0, "id": id, "type": kind, "function_call": map[string]any{"name": name, "arguments": input}}}})...), []byte("\n\n")...)
}
func writeEvents(w http.ResponseWriter, events ...[]byte) {
	for _, b := range events {
		if _, err := w.Write(b); err != nil {
			logs.CtxError(context.Background(), "write fake upstream event: %v", err)
			return
		}
		w.(http.Flusher).Flush()
	}
}

type call struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input string `json:"input"`
}
type decoded struct {
	Text     string
	Complete bool
	Error    bool
	Calls    []call
	Events   int
}

// decode inspects the public protocol, without calling any production translator or repair helper.
func decode(protocol string, raw []byte) decoded {
	out := decoded{}
	blocks := map[int]call{}
	responseCalls := map[string]bool{}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		out.Events++
		if bytes.Equal(data, []byte("[DONE]")) {
			if protocol == "chat_completions" {
				out.Complete = true
			}
			continue
		}
		if !json.Valid(data) {
			out.Error = true
			continue
		}
		j := gjson.ParseBytes(data)
		kind := j.Get("type").String()
		if j.Get("error").Exists() || kind == "error" || kind == "response.failed" {
			out.Error = true
		}
		switch protocol {
		case "responses":
			if kind == "response.output_text.delta" {
				out.Text += j.Get("delta").String()
			}
			if kind == "response.completed" {
				out.Complete = true
			}
			if kind == "response.output_item.done" {
				item := j.Get("item")
				typ := item.Get("type").String()
				if typ == "function_call" || typ == "custom_tool_call" {
					id := item.Get("call_id").String()
					if responseCalls[id] {
						out.Error = true
					}
					responseCalls[id] = true
					value := item.Get("arguments").String()
					if typ == "custom_tool_call" {
						value = item.Get("input").String()
					}
					out.Calls = append(out.Calls, call{id, item.Get("name").String(), value})
				}
			}
		case "messages":
			index := int(j.Get("index").Int())
			if kind == "content_block_start" && j.Get("content_block.type").String() == "tool_use" {
				blocks[index] = call{ID: j.Get("content_block.id").String(), Name: j.Get("content_block.name").String()}
			}
			if kind == "content_block_delta" {
				out.Text += j.Get("delta.text").String()
				if v, ok := blocks[index]; ok {
					v.Input += j.Get("delta.partial_json").String()
					blocks[index] = v
				}
			}
			if kind == "content_block_stop" {
				if v, ok := blocks[index]; ok {
					out.Calls = append(out.Calls, v)
					delete(blocks, index)
				}
			}
			if kind == "message_stop" {
				out.Complete = true
			}
		default:
			out.Text += j.Get("choices.0.delta.content").String()
			for _, item := range j.Get("choices.0.delta.tool_calls").Array() {
				index := int(item.Get("index").Int())
				v := blocks[index]
				v.ID += item.Get("id").String()
				v.Name += item.Get("function.name").String()
				v.Input += item.Get("function.arguments").String()
				blocks[index] = v
			}
		}
	}
	if protocol == "chat_completions" {
		for i := 0; i < len(blocks); i++ {
			out.Calls = append(out.Calls, blocks[i])
		}
	}
	return out
}

func digestBytes(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func wait(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("deadline awaiting " + name)
	}
}

func TestScenario(t *testing.T) {
	id := os.Getenv("TRAE_TEST_CASE")
	if id == "" {
		t.Skip("Run with scripts/trae-test.py to select a frozen integration scenario")
	}
	protocol := os.Getenv("TRAE_TEST_PROTOCOL")
	if protocol == "" {
		protocol = "responses"
	}
	h := newHarness(t, protocol)
	runScenario(h, id, os.Getenv("TRAE_TEST_VARIANT"))
}

func TestDecoderRejectsCorruption(t *testing.T) {
	for _, protocol := range []string{"responses", "messages", "chat_completions"} {
		if got := decode(protocol, []byte("data: {broken}\n\n")); !got.Error || got.Complete {
			t.Fatalf("invalid frame accepted for %s", protocol)
		}
		if got := decode(protocol, []byte("data: {\"error\":{\"message\":\"fixture\"}}\n\n")); !got.Error {
			t.Fatalf("error ignored for %s", protocol)
		}
	}
	if got := decode("responses", []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n")); got.Text != "x" || got.Complete {
		t.Fatal("partial stream fabricated completion")
	}
}
