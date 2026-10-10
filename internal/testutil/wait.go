package testutil

import (
	"testing"
	"time"
)

// WaitFor polls cond until it holds or the timeout expires (fatal then).
// It must be called from the test goroutine (Fatalf).
func WaitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}
