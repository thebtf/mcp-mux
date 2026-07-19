package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fallbackTestAdmission struct {
	closed int
}

func (*fallbackTestAdmission) Verified() bool { return false }

func (admission *fallbackTestAdmission) Close() error {
	admission.closed++
	return nil
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

func TestStartWithFallbackDoesNotRetryUnprovenRollback(t *testing.T) {
	admission := new(fallbackTestAdmission)
	calls := 0

	result, err := StartWithFallback(
		context.Background(),
		EngineRef{ID: "engine-a"},
		EngineRef{ID: "stable-launcher"},
		func(context.Context, EngineRef) (StartResult, error) {
			calls++
			return StartResult{Admission: admission}, ErrStartRollbackUnproven
		},
	)
	if !errors.Is(err, ErrStartRollbackUnproven) {
		t.Fatalf("StartWithFallback() error = %v, want ErrStartRollbackUnproven", err)
	}
	if calls != 1 {
		t.Fatalf("start calls = %d, want 1", calls)
	}
	if result.Child != nil || result.Admission != nil {
		t.Fatalf("result = %#v, want no returned authority after admission cleanup", result)
	}
	if admission.closed != 1 {
		t.Fatalf("admission Close calls = %d, want 1", admission.closed)
	}
}

func TestStartWithFallbackReturnsPartialAuthorityWithoutRetry(t *testing.T) {
	admission := new(fallbackTestAdmission)
	calls := 0

	result, err := StartWithFallback(
		context.Background(),
		EngineRef{ID: "engine-a"},
		EngineRef{ID: "stable-launcher"},
		func(context.Context, EngineRef) (StartResult, error) {
			calls++
			return StartResult{Admission: admission}, errors.New("partial start")
		},
	)
	if err == nil {
		t.Fatal("StartWithFallback() error = nil, want partial-start error")
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
		func(context.Context, EngineRef) (StartResult, error) {
			return StartResult{}, errors.New("start failed")
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
