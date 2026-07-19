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
// generation starts. Rollback-unproven state remains fail-closed; admission
// authority is returned for Run to close but cannot prove process retirement.
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			retainedErr = errors.Join(retainedErr, ctxErr)
		}
		return retained, retainedErr
	}
	if err := ctx.Err(); err != nil {
		return StartResult{}, err
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			retainedErr = errors.Join(retainedErr, ctxErr)
		}
		return retained, retainedErr
	}
	if err := ctx.Err(); err != nil {
		return StartResult{}, err
	}
	return StartResult{}, errFallbackStart
}

func retainFailedStart(result StartResult, err error) (StartResult, error, bool) {
	rollbackUnproven := errors.Is(err, ErrStartRollbackUnproven)
	if rollbackUnproven || startResultHasAuthority(result) {
		retainedErr := errRetainedStart
		if rollbackUnproven {
			retainedErr = errors.Join(retainedErr, ErrStartRollbackUnproven)
		}
		return result, retainedErr, true
	}
	return StartResult{}, nil, false
}
