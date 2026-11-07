package modeltransfer

import (
	"context"
	"testing"
)

func TestCanceledLeaseMonitorDoesNotReportSuccessAsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failures := make(chan error, 1)
	cfg := &Config{}
	cfg.sweepLeases(ctx, failures)
	select {
	case err := <-failures:
		t.Fatalf("cancellation must not race the server shutdown with a monitor result: %v", err)
	default:
	}
}
