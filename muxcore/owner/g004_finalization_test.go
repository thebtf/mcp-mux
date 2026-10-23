package owner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

func TestG004FinalizationReconcilesSettledHandlerAfterZeroGrace(t *testing.T) {
	entered := make(chan context.Context, 1)
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	handlerErr := errors.New("controlled handler completion")
	var gate sync.RWMutex
	o, err := NewOwner(OwnerConfig{
		IPCPath:         testIPCPath(t),
		ProtocolEra:     era.EraModern20260728,
		MaintenanceGate: &gate,
		HandlerFunc: func(ctx context.Context, _ io.Reader, _ io.Writer) error {
			entered <- ctx
			<-released // Cancellation alone does not settle the real handler body.
			return handlerErr
		},
		Logger: testLogger(t),
	})
	if err != nil {
		release()
		t.Fatalf("NewOwner(modern handler): %v", err)
	}
	t.Cleanup(func() {
		release()
		o.materializationFinalizationProbe = nil
		o.Shutdown()
	})

	var handlerCtx context.Context
	select {
	case handlerCtx = <-entered:
	case <-time.After(time.Second):
		t.Fatal("real handler did not reach its release barrier")
	}
	waitForCondition(t, time.Second, func() bool {
		return o.MaterializationState() == MaterializationReady
	}, "modern handler owner did not become ready")
	proc := o.currentUpstream()
	if proc == nil || proc.PID() != 0 {
		t.Fatal("fixture did not retain its actual handler-backed process")
	}

	gate.Lock()
	o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: time.Now()})
	gate.Unlock()
	o.materializationFinalizationProbe = func(got *upstream.Process) error {
		if got != proc {
			return fmt.Errorf("finalization process = %p, want retained %p", got, proc)
		}
		select {
		case <-handlerCtx.Done():
		default:
			return errors.New("zero-grace close did not cancel the real handler context")
		}
		if got.RetirementProven() || got.TreesDead() {
			return errors.New("handler retirement was proven before its body release")
		}
		// This existing seam runs after SoftClose(0) returned its pending warning.
		release()
		select {
		case <-got.Done:
			return nil
		case <-time.After(time.Second):
			return errors.New("released handler did not actually return")
		}
	}

	code, finalized, err := o.FinalizeForRemoval(false, time.Second)
	if !finalized || err != nil || code != 1 {
		t.Errorf("settled handler finalization = (%d, %t, %v), want (1, true, nil)", code, finalized, err)
	}
	select {
	case <-proc.Done:
	default:
		t.Fatal("finalization returned before actual handler completion")
	}
	if !errors.Is(proc.ExitErr, handlerErr) || !proc.RetirementProven() || !proc.TreesDead() || !o.MaintenanceRetired() {
		t.Fatal("actual handler result or retirement proof was lost")
	}
	o.materializationMu.Lock()
	retiring, blockedErr, state := o.retiringProcess, o.materializationBlockedErr, o.materializationState
	o.materializationMu.Unlock()
	if o.currentUpstream() != nil || retiring != nil || blockedErr != nil || state != MaterializationCacheOnly {
		t.Fatalf("settled owner retained process authority: retiring=%p err=%v state=%s", retiring, blockedErr, state)
	}
	select {
	case <-o.Done():
	default:
		t.Fatal("proven owner retirement did not close Done")
	}
}
