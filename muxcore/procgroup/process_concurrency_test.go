package procgroup

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestConcurrentTreeFinalizationIsIdempotent(t *testing.T) {
	p, err := Spawn(treeCmd())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	const callers = 24
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(graceful bool) {
			defer wg.Done()
			if graceful {
				errs <- p.GracefulKill(10 * time.Millisecond)
				return
			}
			errs <- p.Kill()
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent finalizer: %v", err)
		}
	}

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close after concurrent finalization")
	}

	for i := 0; i < callers; i++ {
		if err := p.Kill(); err != nil {
			t.Fatalf("Kill after authority retirement: %v", err)
		}
		if err := p.GracefulKill(time.Millisecond); err != nil {
			t.Fatalf("GracefulKill after authority retirement: %v", err)
		}
	}
}

func TestConcurrentFinalizerWaitsForPublishedDone(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Process) error
	}{
		{name: "kill", call: func(p *Process) error { return p.killPlatform() }},
		{name: "graceful", call: func(p *Process) error { return p.gracefulKillPlatform(time.Millisecond) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Process{done: make(chan struct{})}
			p.platform.state = authorityFinalizing
			p.platform.finalized = make(chan struct{})

			returned := make(chan error, 1)
			go func() { returned <- tc.call(p) }()
			close(p.platform.finalized)

			select {
			case err := <-returned:
				t.Fatalf("finalizer returned before Done was published: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			close(p.done)
			select {
			case err := <-returned:
				if err != nil {
					t.Fatalf("finalizer error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("finalizer did not return after Done")
			}
		})
	}
}

func TestConcurrentFinalizerReturnsPublishedAuthorityError(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Process) error
	}{
		{name: "kill", call: func(p *Process) error { return p.killPlatform() }},
		{name: "graceful", call: func(p *Process) error { return p.gracefulKillPlatform(time.Millisecond) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finalizeErr := errors.New("authority retirement failed")
			p := &Process{done: make(chan struct{})}
			p.platform.state = authorityFinalizing
			p.platform.finalized = make(chan struct{})
			p.platform.finalizeErr = finalizeErr

			returned := make(chan error, 1)
			go func() { returned <- tc.call(p) }()
			close(p.platform.finalized)
			close(p.done)

			select {
			case err := <-returned:
				if !errors.Is(err, finalizeErr) {
					t.Fatalf("finalizer error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("finalizer did not return published authority error")
			}
		})
	}
}

func TestCleanupPlatformReturnsPublishedAuthorityError(t *testing.T) {
	finalizeErr := errors.New("cleanup retirement failed")
	finalized := make(chan struct{})
	close(finalized)
	p := &Process{}
	p.platform.state = authorityFinalized
	p.platform.finalized = finalized
	p.platform.finalizeErr = finalizeErr
	if err := p.cleanupPlatform(); !errors.Is(err, finalizeErr) {
		t.Fatalf("cleanup error = %v", err)
	}
}

func TestPostStartFailureMarksUnprovenRollback(t *testing.T) {
	setupErr := errors.New("platform setup failed")
	rollbackErr := errors.New("rollback failed")
	err := postStartFailure(setupErr, rollbackErr)
	if !errors.Is(err, setupErr) || !errors.Is(err, rollbackErr) || !errors.Is(err, ErrTreeRetirementUnproven) {
		t.Fatalf("post-start failure = %v", err)
	}
}

func TestDisableTreeKillStillTerminatesLeader(t *testing.T) {
	opts := longRunningCmd()
	opts.DisableTree = true
	p, err := Spawn(opts)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("DisableTree leader survived Kill")
	}
}
