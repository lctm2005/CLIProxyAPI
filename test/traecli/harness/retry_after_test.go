package harness

import (
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestTraeCLIRetryAfterHTTPAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		retryAfter string
		wantHeader string
		cooldown   time.Duration
	}{
		{"short_hint", 429, `{"error":"rate limit exceeded"}`, "4", "4", 10 * time.Second},
		{"long_hint", 429, `{"error":"rate limit exceeded"}`, "30", "30", 30 * time.Second},
		{"normalized_rate_limit", 503, `{"error":"throughput limit"}`, "4", "4", 10 * time.Second},
		{"sse_rate_limit", 200, "data: {\"code\":429,\"message\":\"rate limit exceeded\"}\n\n", "4", "4", 10 * time.Second},
		{"missing", 429, `{"error":"rate limit exceeded"}`, "", "", time.Second},
		{"invalid", 429, `{"error":"rate limit exceeded"}`, "invalid", "", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
					h := newHarness(t, "messages")
					h.cfg.DisableCooling = false
					h.manager.SetConfig(h.cfg)
					coolingDisabled := auth.QuotaCooldownDisabledForAuth(nil)
					auth.SetQuotaCooldownDisabled(false)
					t.Cleanup(func() { auth.SetQuotaCooldownDisabled(coolingDisabled) })
					h.setResponder(func(w http.ResponseWriter, r *http.Request, body []byte, n int) {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Retry-After", tc.retryAfter)
						w.Header().Set("X-Upstream-Private", "must-not-forward")
						w.WriteHeader(tc.status)
						writeEvents(w, []byte(tc.body))
					})

					started := time.Now()
					resp, err := h.open(t.Context(), h.payload("first attempt", stream), "fixture-proxy-key")
					checkErr(t, err)
					_, err = io.Copy(io.Discard, resp.Body)
					checkErr(t, err)
					checkErr(t, resp.Body.Close())
					finished := time.Now()
					if resp.StatusCode != http.StatusTooManyRequests {
						t.Fatalf("first status = %d, want 429", resp.StatusCode)
					}
					if got := resp.Header.Get("Retry-After"); got != tc.wantHeader {
						t.Errorf("first Retry-After = %q, want %q", got, tc.wantHeader)
					}
					if got := resp.Header.Get("X-Upstream-Private"); got != "" {
						t.Errorf("private upstream header leaked: %q", got)
					}
					credential, ok := h.manager.GetByID(h.credential.ID)
					if !ok || credential.ModelStates[model] == nil {
						t.Fatal("model cooldown state missing")
					}
					deadline := credential.ModelStates[model].NextRetryAfter
					if deadline.Before(started.Add(tc.cooldown)) || deadline.After(finished.Add(tc.cooldown)) {
						t.Errorf("cooldown deadline = %v, want between %v and %v", deadline, started.Add(tc.cooldown), finished.Add(tc.cooldown))
					}

					resp, err = h.open(t.Context(), h.payload("immediate retry", stream), "fixture-proxy-key")
					checkErr(t, err)
					_, err = io.Copy(io.Discard, resp.Body)
					checkErr(t, err)
					checkErr(t, resp.Body.Close())
					if resp.StatusCode != http.StatusTooManyRequests {
						t.Errorf("cooldown status = %d, want 429", resp.StatusCode)
					}
					if got := resp.Header.Get("Retry-After"); got == "" {
						t.Error("cooldown response missing Retry-After")
					}
					if got := h.count(); got != 1 {
						t.Errorf("upstream requests = %d, want 1 while cooling down", got)
					}
				})
			}
		})
	}
}
