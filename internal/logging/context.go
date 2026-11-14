package logging

import (
	"context"

	"github.com/sirupsen/logrus"
)

// CtxError records an error with the request context when one is available.
func CtxError(ctx context.Context, format string, args ...any) {
	entry := logrus.WithContext(ctx)
	if requestID := GetRequestID(ctx); requestID != "" {
		entry = entry.WithField("request_id", requestID)
	}
	entry.Errorf(format, args...)
}
