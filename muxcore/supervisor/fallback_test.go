package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fallbackTestAdmission struct {
	closed   int
	closeErr error
}

func (*fallbackTestAdmission) Verified() bool { return false }

func (admission *fallbackTestAdmission) Close() error {
	admission.closed++
	return admission.closeErr
}

func TestStartWithFallbackUsesFallbackAfterCleanFailure(t *testing.T) {
	requested := EngineRef{ID: "engine-a"}
	fallback := EngineRef{ID: "stable-launcher"}
	var calls []EngineRef

	result, err := StartWithFallback(context.Background(), requested, fallback, func(_ context.Context, ref EngineRef) (StartResult, error) {
		calls = append(calls, ref)
		if ref == requested {
			return StartResult{}, errors.New("requested start failed")
		}
		return StartResult{Actual: ref}, nil
	})
	if err != nil {
		t.Fatalf("StartWithFallback() error = %v", err)
	}
	if len(calls) != 2 || calls[0] != requested || calls[1] != fallback {
		t.Fatalf("start calls = %#v, want requested then fallback", calls)
	}
	if result.Actual != fallback || !result.Fallback {
		t.Fatalf("result = %#v, want successful fallback", result)
	}
}

func TestStartWithFallbackDoesNotStartFallbackAfterContextCancellation(t *testing.T) {
	const requestedID = "secret-requested-engine"
	requested := EngineRef{ID: requestedID}
	fallback := EngineRef{ID: "fallback-engine"}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	result, err := StartWithFallback(ctx, requested, fallback, func(_ context.Context, ref EngineRef) (StartResult, error) {
		calls++
		if ref == requested {
			cancel()
			return StartResult{}, errors.New("requested failure exposed " + requestedID)
		}
		return StartResult{Actual: ref}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartWithFallback() error = %v, want context cancellation", err)
	}
	if strings.Contains(err.Error(), requestedID) {
		t.Fatalf("StartWithFallback() error leaked requested identity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, want requested attempt only", calls)
	}
	if startResultHasAuthority(result) {
		t.Fatalf("result = %#v, want no authority", result)
	}
}

func TestStartWithFallbackPreservesCancellationFromFallbackAttempt(t *testing.T) {
	const fallbackID = "secret-fallback-engine"
	requested := EngineRef{ID: "requested-engine"}
	fallback := EngineRef{ID: fallbackID}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	result, err := StartWithFallback(ctx, requested, fallback, func(_ context.Context, ref EngineRef) (StartResult, error) {
		calls++
		if ref == requested {
			return StartResult{}, errors.New("requested start failed")
		}
		cancel()
		return StartResult{}, errors.New("fallback failure exposed " + fallbackID)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartWithFallback() error = %v, want context cancellation", err)
	}
	if strings.Contains(err.Error(), fallbackID) {
		t.Fatalf("StartWithFallback() error leaked fallback identity: %v", err)
	}
	if calls != 2 {
		t.Fatalf("start calls = %d, want requested then fallback", calls)
	}
	if startResultHasAuthority(result) {
		t.Fatalf("result = %#v, want no authority", result)
	}
}

func TestStartWithFallbackReturnsAdmissionWithoutRetryForUnprovenRollback(t *testing.T) {
	const requestedID = "secret-requested-engine"
	const fallbackID = "secret-fallback-engine"
	admission := &fallbackTestAdmission{closeErr: errors.New("close exposed " + requestedID + " " + fallbackID)}
	calls := 0

	result, err := StartWithFallback(
		context.Background(),
		EngineRef{ID: requestedID},
		EngineRef{ID: fallbackID},
		func(context.Context, EngineRef) (StartResult, error) {
			calls++
			return StartResult{Admission: admission}, errors.Join(
				ErrStartRollbackUnproven,
				errors.New("rollback exposed "+requestedID+" "+fallbackID),
			)
		},
	)
	if !errors.Is(err, ErrStartRollbackUnproven) {
		t.Fatalf("StartWithFallback() error = %v, want ErrStartRollbackUnproven", err)
	}
	if strings.Contains(err.Error(), requestedID) || strings.Contains(err.Error(), fallbackID) {
		t.Fatalf("StartWithFallback() error leaked identities: %v", err)
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, want 1", calls)
	}
	if result.Admission != admission || result.Fallback {
		t.Fatalf("result = %#v, want requested admission authority", result)
	}
	if admission.closed != 0 {
		t.Fatalf("admission Close calls = %d, want supervisor finalization to own cleanup", admission.closed)
	}
}

func TestStartWithFallbackReturnsPartialAuthorityWithoutRetry(t *testing.T) {
	const requestedID = "secret-requested-engine"
	const fallbackID = "secret-fallback-engine"
	admission := new(fallbackTestAdmission)
	calls := 0

	result, err := StartWithFallback(
		context.Background(),
		EngineRef{ID: requestedID},
		EngineRef{ID: fallbackID},
		func(context.Context, EngineRef) (StartResult, error) {
			calls++
			return StartResult{Admission: admission}, errors.New("partial start " + requestedID + " " + fallbackID)
		},
	)
	if err == nil {
		t.Fatal("StartWithFallback() error = nil, want partial-start error")
	}
	if strings.Contains(err.Error(), requestedID) || strings.Contains(err.Error(), fallbackID) {
		t.Fatalf("StartWithFallback() error leaked identities: %v", err)
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, want 1", calls)
	}
	if result.Admission != admission || result.Fallback {
		t.Fatalf("result = %#v, want requested partial authority", result)
	}
	if admission.closed != 0 {
		t.Fatalf("admission Close calls = %d, want supervisor finalization to own cleanup", admission.closed)
	}
}

func TestStartWithFallbackRedactsFallbackPartialAuthority(t *testing.T) {
	const requestedID = "secret-requested-engine"
	const fallbackID = "secret-fallback-engine"
	requested := EngineRef{ID: requestedID}
	fallback := EngineRef{ID: fallbackID}
	admission := new(fallbackTestAdmission)
	calls := 0

	result, err := StartWithFallback(context.Background(), requested, fallback, func(_ context.Context, ref EngineRef) (StartResult, error) {
		calls++
		if ref == requested {
			return StartResult{}, errors.New("requested failure exposed " + requestedID)
		}
		return StartResult{Admission: admission}, errors.New("fallback failure exposed " + fallbackID)
	})
	if err == nil {
		t.Fatal("StartWithFallback() error = nil, want fallback partial-start error")
	}
	if strings.Contains(err.Error(), requestedID) || strings.Contains(err.Error(), fallbackID) {
		t.Fatalf("StartWithFallback() error leaked identities: %v", err)
	}
	if calls != 2 {
		t.Fatalf("start calls = %d, want 2", calls)
	}
	if result.Admission != admission || !result.Fallback {
		t.Fatalf("result = %#v, want fallback partial authority", result)
	}
}

func TestStartWithFallbackRejectsSameIdentity(t *testing.T) {
	calls := 0
	_, err := StartWithFallback(
		context.Background(),
		EngineRef{ID: "same"},
		EngineRef{ID: "same"},
		func(context.Context, EngineRef) (StartResult, error) {
			calls++
			return StartResult{}, errors.New("start failed")
		},
	)
	if err == nil {
		t.Fatal("StartWithFallback() error = nil, want no-distinct-fallback error")
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, want 1", calls)
	}
}

func TestStartWithFallbackRedactsFailedIdentities(t *testing.T) {
	const requestedID = "secret-requested-engine"
	const fallbackID = "secret-fallback-engine"
	_, err := StartWithFallback(
		context.Background(),
		EngineRef{ID: requestedID},
		EngineRef{ID: fallbackID},
		func(_ context.Context, ref EngineRef) (StartResult, error) {
			return StartResult{}, errors.New("start failed for " + ref.ID)
		},
	)
	if err == nil {
		t.Fatal("StartWithFallback() error = nil, want fallback failure")
	}
	if strings.Contains(err.Error(), requestedID) || strings.Contains(err.Error(), fallbackID) {
		t.Fatalf("StartWithFallback() error leaked identities: %v", err)
	}
}

func TestStartWithFallbackRejectsNilStarter(t *testing.T) {
	if _, err := StartWithFallback(context.Background(), EngineRef{}, EngineRef{}, nil); err == nil {
		t.Fatal("StartWithFallback() error = nil, want nil starter error")
	}
}
