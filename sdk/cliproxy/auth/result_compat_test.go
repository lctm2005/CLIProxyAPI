package auth_test

import (
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestResultUpstreamUnkeyedLiteralCompatibility(t *testing.T) {
	result := cliproxyauth.Result{"auth", "provider", "model", "route", true, nil, false, nil, cliproxyexecutor.Options{}, false}

	if result.AuthID != "auth" || result.Provider != "provider" || result.Model != "model" || result.RouteModel != "route" || !result.Success {
		t.Fatalf("unexpected result fields: %#v", result)
	}
}

func TestModelStateLegacyUnkeyedLiteralCompatibility(t *testing.T) {
	state := cliproxyauth.ModelState{cliproxyauth.StatusActive, "", false, time.Time{}, nil, cliproxyauth.QuotaState{}, time.Time{}}

	if state.Status != cliproxyauth.StatusActive || state.Unavailable {
		t.Fatalf("unexpected model state fields: %#v", state)
	}
}
