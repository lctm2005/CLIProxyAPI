package helps

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNewTraeHTTPClientUsesContextRoundTripper(t *testing.T) {
	t.Parallel()

	called := false
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Hostname() != "api.enterprise.trae.cn" {
			t.Fatalf("hostname = %q, want api.enterprise.trae.cn", req.URL.Hostname())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	}))

	client := NewTraeHTTPClient(ctx, nil, nil, 0)
	resp, err := client.Get("https://api.enterprise.trae.cn/api/ide/v2/llm_raw_chat")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !called {
		t.Fatal("expected context RoundTripper to handle TRAE request")
	}
}

func TestWriteTraeRawHTTPRequestMatchesOfficialHeaderShape(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.enterprise.trae.cn/api/ide/v2/llm_raw_chat", strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	req.Header.Set("Version", "0.200.17")
	req.Header.Set("X-App-Id", "app")
	req.Header.Set("X-IDE-Function", "traecli_next")
	req.Header.Set("X-IDE-Version-Code", "20260713")
	req.Header.Set("X-Flow-Traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-aaaaaaaaaaaaaaaa-01")
	req.Header.Set("X-Custom-Repo-Urls", "https://github.com/router-for-me/CLIProxyAPI")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Cloud-CLI-JWT token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Originator", "codex_exec")
	req.Header.Set("User-Agent", "codex_exec/0.200.17")
	req.Header.Set("Accept-Encoding", "gzip")

	var buf bytes.Buffer
	if err := writeTraeRawHTTPRequest(&buf, req); err != nil {
		t.Fatalf("writeTraeRawHTTPRequest returned error: %v", err)
	}
	raw := buf.String()
	wantPrefix := "POST /api/ide/v2/llm_raw_chat HTTP/1.1\r\n" +
		"version: 0.200.17\r\n" +
		"x-app-id: app\r\n" +
		"x-ide-function: traecli_next\r\n" +
		"x-ide-version-code: 20260713\r\n" +
		"x-flow-traceparent: 00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-aaaaaaaaaaaaaaaa-01\r\n" +
		"x-custom-repo-urls: https://github.com/router-for-me/CLIProxyAPI\r\n" +
		"accept: text/event-stream\r\n" +
		"authorization: Cloud-CLI-JWT token\r\n" +
		"content-type: application/json\r\n" +
		"originator: codex_exec\r\n" +
		"user-agent: codex_exec/0.200.17\r\n" +
		"host: api.enterprise.trae.cn\r\n" +
		"content-length: 11\r\n\r\n"
	if !strings.HasPrefix(raw, wantPrefix) {
		t.Fatalf("request prefix mismatch\n got: %q\nwant: %q", raw, wantPrefix)
	}
	if strings.Contains(strings.ToLower(raw), "accept-encoding:") {
		t.Fatalf("request should not include accept-encoding, got %q", raw)
	}
	if !strings.HasSuffix(raw, `{"ok":true}`) {
		t.Fatalf("request body missing, got %q", raw)
	}
}
