package owner

import (
	"fmt"
	"sync"
	"testing"
)

func TestPreRegisterPurgedWhenIsolatedClassificationClosesListener(t *testing.T) {
	o := NewTestOwner("", "isolated-owner")

	if !o.PreRegister("token", "/project", nil) {
		t.Fatal("PreRegister rejected while owner was accepting")
	}
	o.classifyFromCapabilities([]byte(`{"result":{"capabilities":{"x-mux":{"sharing":"isolated"}}}}`))

	if got := o.SessionMgr().PendingCount(); got != 0 {
		t.Fatalf("PendingCount after isolated classification = %d, want 0", got)
	}
	if o.PreRegister("late-token", "/project", nil) {
		t.Fatal("PreRegister accepted after listener closed")
	}
}

func TestPreRegisterConcurrentWithCloseLeavesNoPending(t *testing.T) {
	o := NewTestOwner("", "closing-owner")
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			o.PreRegister(fmt.Sprintf("token-%d", i), "/project", nil)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		o.closeListener()
	}()

	close(start)
	wg.Wait()

	if got := o.SessionMgr().PendingCount(); got != 0 {
		t.Fatalf("PendingCount after concurrent close/register = %d, want 0", got)
	}
	if o.PreRegister("post-close", "/project", nil) {
		t.Fatal("PreRegister accepted after concurrent close")
	}
}
