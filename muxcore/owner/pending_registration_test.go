package owner

import (
	"fmt"
	"sync"
	"testing"
)

func TestPreRegisterPurgedWhenIsolatedClassificationRestrictsFreshAdmission(t *testing.T) {
	o := NewTestOwner("", "isolated-owner")

	if !o.PreRegisterInitial("initial-token", "/project", nil) {
		t.Fatal("PreRegisterInitial rejected the creating shim")
	}
	if !o.PreRegister("provisional-token", "/project", nil) {
		t.Fatal("PreRegister rejected provisional fan-in before classification")
	}
	o.classifyFromCapabilities([]byte(`{"result":{"capabilities":{"x-mux":{"sharing":"isolated"}}}}`))

	if got := o.SessionMgr().PendingCount(); got != 1 {
		t.Fatalf("PendingCount after isolated classification = %d, want creating token only", got)
	}
	if !o.SessionMgr().IsPreRegistered("initial-token") {
		t.Fatal("isolated classification revoked the creating shim token")
	}
	if o.SessionMgr().IsPreRegistered("provisional-token") {
		t.Fatal("isolated classification retained a provisional fan-in token")
	}
	if !o.IsAccepting() {
		t.Fatal("isolated classification closed the listener needed for token reconnect")
	}
	if o.PreRegister("late-token", "/project", nil) {
		t.Fatal("PreRegister accepted a fresh token after isolated classification")
	}
	if o.PreRegisterInitial("other-initial", "/project", nil) {
		t.Fatal("PreRegisterInitial accepted a second creating token")
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
