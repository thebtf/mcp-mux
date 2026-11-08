package session

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// queueTestWriter blocks the actual session writer until the test releases it.
type queueTestWriter struct {
	started chan struct{}
	release chan struct{}
	writes  chan string
	once    sync.Once
}

func (w *queueTestWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	w.writes <- string(p)
	return len(p), nil
}

func queueTestSession(t *testing.T) (*Session, *queueTestWriter, func()) {
	t.Helper()
	w := &queueTestWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
		writes:  make(chan string, notificationBufferSize+1),
	}
	s := NewSession(strings.NewReader(""), w)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(w.release) }) }
	t.Cleanup(func() {
		release()
		s.Close()
	})
	if err := s.EnqueueNotification([]byte("in-flight")); err != nil {
		t.Fatalf("enqueue initial notification: %v", err)
	}
	queueTestWait(t, w.started, "writer to block")
	return s, w, release
}

func queueTestWait(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestNotificationQueueRetainsAcceptedMessages(t *testing.T) {
	s, w, release := queueTestSession(t)
	for i := range notificationBufferSize {
		data := []byte(fmt.Sprintf("queued-%d", i))
		if err := s.EnqueueNotification(data); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		data[0] = 'X'
	}
	if err := s.EnqueueNotification([]byte("rejected")); err == nil {
		t.Fatal("enqueue succeeded on a full queue")
	}

	release()
	for i := -1; i < notificationBufferSize; i++ {
		want := "in-flight\n"
		if i >= 0 {
			want = fmt.Sprintf("queued-%d\n", i)
		}
		select {
		case got := <-w.writes:
			if got != want {
				t.Fatalf("delivery %d = %q, want %q", i, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for delivery %d", i)
		}
	}
	s.Close()
	if err := s.EnqueueNotification([]byte("after-close")); err == nil {
		t.Fatal("enqueue succeeded after Close returned")
	}
}

func TestNotificationQueueMixedProducersClose(t *testing.T) {
	// This is a bounded scheduling stress test: the problematic interleaving is
	// between the legacy eviction and send, and cannot be forced by the API.
	const rounds, producers, operations = 32, 16, 256
	for round := range rounds {
		func() {
			s, _, release := queueTestSession(t)
			for range notificationBufferSize {
				if err := s.EnqueueNotification([]byte("fill")); err != nil {
					t.Fatalf("round %d: filling queue: %v", round, err)
				}
			}

			start := make(chan struct{})
			half := make(chan struct{}, producers)
			resume := make(chan struct{})
			var resumeOnce sync.Once
			resumeProducers := func() { resumeOnce.Do(func() { close(resume) }) }
			defer resumeProducers()
			finished := make(chan struct{})
			var wg sync.WaitGroup
			for p := range producers {
				wg.Add(1)
				go func(legacy bool) {
					defer wg.Done()
					<-start
					for i := range operations {
						if i == operations/2 {
							half <- struct{}{}
							<-resume
						}
						if legacy {
							s.SendNotification([]byte("legacy"))
						} else {
							_ = s.EnqueueNotification([]byte("new"))
						}
					}
				}(p%2 == 0)
			}
			go func() { wg.Wait(); close(finished) }()
			close(start)
			for range producers {
				queueTestWait(t, half, fmt.Sprintf("round %d producer midpoint", round))
			}
			resumeProducers()
			closed := make(chan struct{})
			go func() { s.Close(); close(closed) }()
			queueTestWait(t, closed, fmt.Sprintf("round %d Close", round))
			if err := s.EnqueueNotification([]byte("after-close")); err == nil {
				t.Fatalf("round %d: enqueue succeeded after Close returned", round)
			}
			queueTestWait(t, finished, fmt.Sprintf("round %d producers to return", round))
			release()
		}()
	}
}
