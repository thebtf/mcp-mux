package daemon

import (
	"errors"
	"fmt"
	"testing"

	"github.com/thejerf/suture/v4"
)

// unknownEvent is a stub that satisfies suture.Event (fmt.Stringer + Type + Map)
// but is not one of the known concrete event types. Used to test the default branch.
type unknownEvent struct{}

func (unknownEvent) String() string                  { return "unknown-event" }
func (unknownEvent) Type() suture.EventType          { return suture.EventType(999) }
func (unknownEvent) Map() map[string]interface{}     { return map[string]interface{}{} }

// Compile-time check: unknownEvent must implement suture.Event.
var _ suture.Event = unknownEvent{}

// TestClassifyPanic: EventServicePanic → TermDaemonPanic regardless of hint.
func TestClassifyPanic(t *testing.T) {
	ev := suture.EventServicePanic{ServiceName: "test-svc"}

	got := classifyTermination(ev, HintNone)
	if got != TermDaemonPanic {
		t.Errorf("EventServicePanic HintNone: got %v, want TermDaemonPanic", got)
	}

	// Hint must be ignored for panics.
	got2 := classifyTermination(ev, HintPlannedHandoff)
	if got2 != TermDaemonPanic {
		t.Errorf("EventServicePanic HintPlannedHandoff: got %v, want TermDaemonPanic", got2)
	}
}

// TestClassifyCleanExitWithHint: EventServiceTerminate Err=nil + each non-None hint maps correctly.
func TestClassifyCleanExitWithHint(t *testing.T) {
	cases := []struct {
		hint TerminationHint
		want TerminationCause
	}{
		{HintPlannedHandoff, TermPlannedHandoff},
		{HintOperatorStop, TermOperatorStop},
		{HintIdleEviction, TermIdleEviction},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("hint=%v", tc.hint), func(t *testing.T) {
			ev := suture.EventServiceTerminate{ServiceName: "test-svc", Err: nil}
			got := classifyTermination(ev, tc.hint)
			if got != tc.want {
				t.Errorf("hint=%v: got %v, want %v", tc.hint, got, tc.want)
			}
		})
	}
}

// TestClassifyCleanExitNoHint: Err=nil + HintNone → TermOperatorStop (default for clean exit).
func TestClassifyCleanExitNoHint(t *testing.T) {
	ev := suture.EventServiceTerminate{ServiceName: "test-svc", Err: nil}
	got := classifyTermination(ev, HintNone)
	if got != TermOperatorStop {
		t.Errorf("clean exit no hint: got %v, want TermOperatorStop", got)
	}
}

// TestClassifyErrDoNotRestart: Err=suture.ErrDoNotRestart + HintNone → TermOperatorStop.
func TestClassifyErrDoNotRestart(t *testing.T) {
	ev := suture.EventServiceTerminate{
		ServiceName: "test-svc",
		Err:         suture.ErrDoNotRestart,
	}
	got := classifyTermination(ev, HintNone)
	if got != TermOperatorStop {
		t.Errorf("ErrDoNotRestart no hint: got %v, want TermOperatorStop", got)
	}
}

// TestClassifyCrash: non-nil non-ErrDoNotRestart error without hint → TermUpstreamCrash;
// same error with HintPlannedHandoff → TermPlannedHandoff (hint wins over error content).
func TestClassifyCrash(t *testing.T) {
	crashErr := errors.New("upstream died")
	ev := suture.EventServiceTerminate{ServiceName: "test-svc", Err: crashErr}

	// No hint: error indicates crash.
	got := classifyTermination(ev, HintNone)
	if got != TermUpstreamCrash {
		t.Errorf("crash no hint: got %v, want TermUpstreamCrash", got)
	}

	// Hint wins: caller knows this was their planned tear-down despite the error.
	got2 := classifyTermination(ev, HintPlannedHandoff)
	if got2 != TermPlannedHandoff {
		t.Errorf("crash with HintPlannedHandoff: got %v, want TermPlannedHandoff", got2)
	}
}

// TestUnknownEvent: an event type not handled by the classifier → TermUnknown.
func TestUnknownEvent(t *testing.T) {
	got := classifyTermination(unknownEvent{}, HintNone)
	if got != TermUnknown {
		t.Errorf("unknown event: got %v, want TermUnknown", got)
	}
}

// TestTerminationHintForEvent_ResolvesPerOwnerHint verifies that
// terminationHintForEvent parses the service name, finds the matching
// OwnerEntry, and returns its stored terminationHint. Fixes the
// CodeRabbit concern that supervisorEventHook was hard-coding HintNone.
func TestTerminationHintForEvent_ResolvesPerOwnerHint(t *testing.T) {
	d := &Daemon{owners: map[string]*OwnerEntry{}}
	sid := "aabbccdd-hint-lookup-test"
	d.owners[sid] = &OwnerEntry{
		ServerID:        sid,
		Command:         "echo",
		terminationHint: HintPlannedHandoff,
	}

	// Service name format from suture matches cleanupDeadOwner parsing:
	// "owner[XXXXXXXX command args]" — first 8 hex chars are the sid prefix.
	ev := suture.EventServiceTerminate{
		ServiceName: fmt.Sprintf("owner[%s echo]", sid[:8]),
	}

	got := d.terminationHintForEvent(ev)
	if got != HintPlannedHandoff {
		t.Errorf("terminationHintForEvent: got %v, want HintPlannedHandoff", got)
	}

	// Panic events also carry a service name — same resolution path.
	evPanic := suture.EventServicePanic{
		ServiceName: fmt.Sprintf("owner[%s echo]", sid[:8]),
	}
	got2 := d.terminationHintForEvent(evPanic)
	if got2 != HintPlannedHandoff {
		t.Errorf("terminationHintForEvent panic event: got %v, want HintPlannedHandoff", got2)
	}
}

// TestTerminationHintForEvent_UnknownService returns HintNone when the
// service name does not match any registered owner.
func TestTerminationHintForEvent_UnknownService(t *testing.T) {
	d := &Daemon{owners: map[string]*OwnerEntry{}}
	ev := suture.EventServiceTerminate{ServiceName: "owner[deadbeef echo]"}
	got := d.terminationHintForEvent(ev)
	if got != HintNone {
		t.Errorf("unknown service: got %v, want HintNone", got)
	}
}

// TestTerminationHintForEvent_MalformedServiceName returns HintNone for
// events whose service name does not match the "owner[XXXXXXXX ...]"
// convention (defensive; covers future unrelated suture services).
func TestTerminationHintForEvent_MalformedServiceName(t *testing.T) {
	d := &Daemon{owners: map[string]*OwnerEntry{}}

	// Missing "owner[" prefix.
	ev1 := suture.EventServiceTerminate{ServiceName: "not-an-owner"}
	if got := d.terminationHintForEvent(ev1); got != HintNone {
		t.Errorf("no owner prefix: got %v, want HintNone", got)
	}

	// Prefix present but no closing brace/space.
	ev2 := suture.EventServiceTerminate{ServiceName: "owner[aabbccdd"}
	if got := d.terminationHintForEvent(ev2); got != HintNone {
		t.Errorf("no space after sid: got %v, want HintNone", got)
	}

	// Event type with no service name at all.
	if got := d.terminationHintForEvent(unknownEvent{}); got != HintNone {
		t.Errorf("unknownEvent: got %v, want HintNone", got)
	}
}
