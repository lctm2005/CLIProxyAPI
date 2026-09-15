package helps

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	logs "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// ParseRetryAfter parses standard delay-seconds or HTTP-date values. Missing,
// invalid or overflowing values have no hint; zero and past dates mean no wait.
func ParseRetryAfter(ctx context.Context, raw string, now time.Time) *time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		seconds, errParse := strconv.ParseInt(raw, 10, 64)
		if errParse != nil {
			logs.CtxError(ctx, "retry-after: delay-seconds exceeds integer range")
			return nil
		}
		if seconds > math.MaxInt64/int64(time.Second) {
			logs.CtxError(ctx, "retry-after: delay-seconds exceeds duration range")
			return nil
		}
		delay := time.Duration(seconds) * time.Second
		return &delay
	}
	deadline, errParse := http.ParseTime(raw)
	if errParse != nil {
		logs.CtxError(ctx, "retry-after: invalid delay-seconds or HTTP-date")
		return nil
	}
	delay := max(deadline.Sub(now), 0)
	return &delay
}
