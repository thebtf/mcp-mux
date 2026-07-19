package supervisor

import (
	"context"
	"errors"
)

var (
	errNilFallbackStarter = errors.New("supervisor: nil fallback starter")
	errNoDistinctFallback = errors.New("supervisor: requested child start failed and no distinct fallback is available")
	errFallbackStart      = errors.New("supervisor: requested child and fallback start failed")
	errRetainedStart      = errors.New("supervisor: child start failed with retained authority")
)

// StartWithFallback starts requested first, then fallback only after a clean
// requested-start failure that returned no child or admission authority.
// Engine identities remain opaque and are never included in returned errors.
// Failed attempts that retain authority are returned immediately with a fixed
// classification so Run can finalize the exact authority before any later
// generation starts. Admission-only rollback-unproven state is cleaned here;
// failed cleanup returns that admission authority for Run to finalize.
func StartWithFallback(ctx context.Context, requested, fallback EngineRef, start StartFunc) (StartResult, error) {
	if start == nil {
		return StartResult{}, errNilFallbackStarter
	}

	result, err := start(ctx, requested)
	result.Fallback = false
	if err == nil {
		return result, nil
	}
	if retained, retainedErr, terminal := retainFailedStart(result, err); terminal {
		return retained, retainedErr
	}
	if requested == fallback {
		return StartResult{}, errNoDistinctFallback
	}

	result, err = start(ctx, fallback)
	result.Fallback = true
	if err == nil {
		return result, nil
	}
	if retained, retainedErr, terminal := retainFailedStart(result, err); terminal {
		return retained, retainedErr
	}
	return StartResult{}, errFallbackStart
}

func retainFailedStart(result StartResult, err error) (StartResult, error, bool) {
	rollbackUnproven := errors.Is(err, ErrStartRollbackUnproven)
	if rollbackUnproven && isNilInterface(result.Child) {
		if isNilInterface(result.Admission) {
			return StartResult{}, ErrStartRollbackUnproven, true
		}
		if closeErr := result.Admission.Close(); closeErr == nil {
			return StartResult{}, ErrStartRollbackUnproven, true
		}
		return result, errors.Join(errRetainedStart, ErrStartRollbackUnproven), true
	}
	if rollbackUnproven || startResultHasAuthority(result) {
		retainedErr := errRetainedStart
		if rollbackUnproven {
			retainedErr = errors.Join(retainedErr, ErrStartRollbackUnproven)
		}
		return result, retainedErr, true
	}
	return StartResult{}, nil, false
}
