package helps

import (
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.September, 14, 8, 0, 0, 250_000_000, time.UTC)
	for _, tc := range []struct {
		name string
		raw  string
		want *time.Duration
	}{
		{"seconds", "4", new(4 * time.Second)},
		{"whitespace", " \t4\t ", new(4 * time.Second)},
		{"zero", "0", new(time.Duration(0))},
		{"leading_zeroes", "0004", new(4 * time.Second)},
		{"http_date", now.Add(4 * time.Second).Format(http.TimeFormat), new(3750 * time.Millisecond)},
		{"past_date", now.Add(-time.Second).Format(http.TimeFormat), new(time.Duration(0))},
		{"largest_seconds", strconv.FormatInt(math.MaxInt64/int64(time.Second), 10), new(time.Duration(math.MaxInt64/int64(time.Second)) * time.Second)},
		{"missing", "", nil},
		{"blank", " \t", nil},
		{"invalid", "invalid", nil},
		{"negative", "-1", nil},
		{"plus_sign", "+4", nil},
		{"fractional", "1.5", nil},
		{"multiple_values", "4, 8", nil},
		{"duration_overflow", "9223372037", nil},
		{"integer_overflow", "9223372036854775808", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseRetryAfter(t.Context(), tc.raw, now)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("delay = %v, want nil", *got)
				}
			} else if got == nil || *got != *tc.want {
				t.Fatalf("delay = %v, want %v", got, *tc.want)
			}
		})
	}
}
