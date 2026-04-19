package daemon

import (
	"errors"

	"github.com/thejerf/suture/v4"
)

// TerminationCause classifies the reason an owner's Serve returned.
type TerminationCause int

const (
	TermUnknown        TerminationCause = iota
	TermPlannedHandoff                  // graceful restart / binary upgrade
	TermUpstreamCrash                   // upstream process died unexpectedly
	TermOperatorStop                    // explicit Remove or clean nil exit
	TermIdleEviction                    // reaper evicted an idle owner
	TermDaemonPanic                     // supervisor caught a panic
)

// String returns the lowercase snake_case name of the termination cause.
// Used by observability (NFR-4) in T025.
func (t TerminationCause) String() string {
	switch t {
	case TermPlannedHandoff:
		return "planned_handoff"
	case TermUpstreamCrash:
		return "upstream_crash"
	case TermOperatorStop:
		return "operator_stop"
	case TermIdleEviction:
		return "idle_eviction"
	case TermDaemonPanic:
		return "daemon_panic"
	default:
		return "unknown"
	}
}

// TerminationHint is an optional caller-provided signal that the
// classifier cannot derive from the suture event alone (e.g., operator
// actions and idle evictions are issued from outside the supervisor).
type TerminationHint int

const (
	HintNone           TerminationHint = iota
	HintPlannedHandoff                 // set by HandleGracefulRestart before shutdown
	HintOperatorStop                   // set by Remove when invoked from control plane
	HintIdleEviction                   // set by reaper before SoftRemove
)

// classifyTermination maps a supervisor event (and optional hint) to a cause.
// Rules (evaluated in order):
//  1. EventServicePanic → TermDaemonPanic (hint is ignored)
//  2. EventServiceTerminate:
//     a. If hint != HintNone → use hint as cause (hint wins over error)
//     b. If Err is nil OR errors.Is(Err, suture.ErrDoNotRestart) → TermOperatorStop
//        (clean exit default; without a hint, clean exit = caller explicitly stopped service)
//     c. Otherwise (non-nil, non-ErrDoNotRestart Err) → TermUpstreamCrash
//  3. Unknown event type → TermUnknown
//
// Note: suture.EventServiceTerminate.Err is typed interface{} in suture v4.
// A type assertion to error is required before calling errors.Is.
func classifyTermination(ev suture.Event, hint TerminationHint) TerminationCause {
	switch e := ev.(type) {
	case suture.EventServicePanic:
		_ = e
		return TermDaemonPanic

	case suture.EventServiceTerminate:
		if hint != HintNone {
			switch hint {
			case HintPlannedHandoff:
				return TermPlannedHandoff
			case HintOperatorStop:
				return TermOperatorStop
			case HintIdleEviction:
				return TermIdleEviction
			}
		}
		// suture v4 types Err as interface{} — assert before errors.Is.
		errVal, _ := e.Err.(error)
		if e.Err == nil || errors.Is(errVal, suture.ErrDoNotRestart) {
			return TermOperatorStop
		}
		return TermUpstreamCrash

	default:
		return TermUnknown
	}
}
