package supervisor

import (
	"context"
	"testing"
	"time"
)

func TestGenerationCancelJoinsBlockedPumpAndReaperSenders(t *testing.T) {
	child := newTestChild()
	events := make(chan any)
	generation, err := newGeneration(
		context.Background(),
		1,
		EngineRef{ID: "engine"},
		StartResult{Child: child, Actual: EngineRef{ID: "engine"}},
		1024,
		events,
	)
	if err != nil {
		t.Fatal(err)
	}
	child.finish(0, nil)
	select {
	case <-generation.exitDone:
	case <-time.After(time.Second):
		t.Fatal("child terminal result was not observed")
	}
	select {
	case <-generation.reapDone:
		t.Fatal("reaper sender unexpectedly escaped an unread event channel")
	default:
	}
	generation.cancel()
	select {
	case <-generation.reapDone:
	case <-time.After(time.Second):
		t.Fatal("generation cancellation did not join reaper sender")
	}
	select {
	case <-generation.pumpDone:
	case <-time.After(time.Second):
		t.Fatal("generation cancellation did not join stdout pump")
	}
}
