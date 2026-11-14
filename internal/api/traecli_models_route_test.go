package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestTraeCLIClaudeModelsRequestDetection(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{name: "anthropic version", header: http.Header{"Anthropic-Version": {"2023-06-01"}}, want: true},
		{name: "x api key", header: http.Header{"X-Api-Key": {"proxy-key"}}, want: true},
		{name: "claude code user agent", header: http.Header{"User-Agent": {"Claude-Code/2.0"}}, want: true},
		{name: "openai", header: http.Header{"Authorization": {"Bearer proxy-key"}}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header = test.header
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = req
			if got := isAnthropicModelsRequest(ctx); got != test.want {
				t.Fatalf("isAnthropicModelsRequest() = %v, want %v", got, test.want)
			}
		})
	}
}
