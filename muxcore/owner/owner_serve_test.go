package owner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thejerf/suture/v4"
)

func TestOwnerServe_BlocksUntilCancel(t *testing.T) {
	o := newMinimalOwner()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- o.Serve(ctx) }()

	select {
	case err := <-errCh:
		t.Fatalf("Serve returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
	select {
	case <-o.Done():
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not close owner done channel")
	}
}

func TestOwnerServe_ReturnsErrDoNotRestartOnCleanShutdown(t *testing.T) {
	o := newMinimalOwner()
	errCh := make(chan error, 1)
	go func() { errCh <- o.Serve(context.Background()) }()

	select {
	case err := <-errCh:
		t.Fatalf("Serve returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	o.Shutdown()
	select {
	case err := <-errCh:
		if !errors.Is(err, suture.ErrDoNotRestart) {
			t.Fatalf("Serve returned %v, want suture.ErrDoNotRestart", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}

func TestOwnerServe_CacheOnlyOwnerIsHealthy(t *testing.T) {
	o := newMinimalOwner()
	o.materializationState = MaterializationCacheOnly
	o.upstream = nil
	errCh := make(chan error, 1)
	go func() { errCh <- o.Serve(context.Background()) }()

	select {
	case err := <-errCh:
		t.Fatalf("cache-only Serve returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	o.Shutdown()
	select {
	case err := <-errCh:
		if !errors.Is(err, suture.ErrDoNotRestart) {
			t.Fatalf("cache-only Serve returned %v after Shutdown, want ErrDoNotRestart", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cache-only Serve did not return after Shutdown")
	}
}

func TestOwnerServe_ImplementsServiceInterface(t *testing.T) {
	var _ suture.Service = (*Owner)(nil)
}
