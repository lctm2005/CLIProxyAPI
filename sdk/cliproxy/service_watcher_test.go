package cliproxy

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestIsWatcherResourceExhaustion(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "emfile", err: syscall.EMFILE, want: true},
		{name: "wrapped enospc", err: fmt.Errorf("watcher: %w", syscall.ENOSPC), want: true},
		{name: "other", err: errors.New("permission denied"), want: false},
		{name: "nil", err: nil, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isWatcherResourceExhaustion(test.err); got != test.want {
				t.Fatalf("isWatcherResourceExhaustion() = %v, want %v", got, test.want)
			}
		})
	}
}
