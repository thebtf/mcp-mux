package supervisor

import (
	"context"
	"errors"
)

var (
	errNilFallbackStarter = errors.New("supervisor: nil fallback starter")
	errNoDistinctFallback = errors.New("supervisor: requested child start failed and no distinct fallback is available")
	errFallbackStart      = errors.New("supervisor: requested child and fallback start failed")
)

// StartWithFallback starts requested first, then fallback only after a clean
// requested-start failure that returned no child or admission authority.
// Engine identities remain opaque and are never included in returned errors.
// A partial or rollback-unproven start is returned immediately so Run can
// finalize the exact authority before any later generation starts.
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
	if errors.Is(err, ErrStartRollbackUnproven) && isNilInterface(result.Child) {
		if !isNilInterface(result.Admission) {
			err = errors.Join(err, result.Admission.Close())
		}
		return StartResult{}, err, true
	}
	if errors.Is(err, ErrStartRollbackUnproven) || startResultHasAuthority(result) {
		return result, err, true
	}
	return StartResult{}, nil, false
}
