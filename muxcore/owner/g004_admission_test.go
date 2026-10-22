package owner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
)

type g004AdmissionHandler struct {
	request      func(context.Context, []byte) ([]byte, error)
	notification func(context.Context, []byte)
	plainCalls   atomic.Int64
	metaCalls    atomic.Int64
	frames       atomic.Int64
}

func (h *g004AdmissionHandler) HandleRequest(ctx context.Context, _ muxcore.ProjectContext, raw []byte) ([]byte, error) {
	h.plainCalls.Add(1)
	return h.request(ctx, raw)
}

func (h *g004AdmissionHandler) HandleNotification(ctx context.Context, _ muxcore.ProjectContext, raw []byte) {
	h.plainCalls.Add(1)
	h.notification(ctx, raw)
}

type g004AdmissionMetaHandler struct {
	*g004AdmissionHandler
}

func (h *g004AdmissionMetaHandler) HandleRequestWithSessionMeta(ctx context.Context, _ muxcore.ProjectContext, _ muxcore.SessionMeta, raw []byte) ([]byte, error) {
	h.metaCalls.Add(1)
	return h.request(ctx, raw)
}

func (h *g004AdmissionMetaHandler) HandleNotificationWithSessionMeta(ctx context.Context, _ muxcore.ProjectContext, _ muxcore.SessionMeta, raw []byte) {
	h.metaCalls.Add(1)
	h.notification(ctx, raw)
}

type g004AdmissionBarrier struct {
	entered chan context.Context
	release chan struct{}
	unblock func()
	calls   atomic.Int64
	active  atomic.Int64
	peak    atomic.Int64
}

func newG004AdmissionBarrier(t *testing.T) *g004AdmissionBarrier {
	t.Helper()
	b := &g004AdmissionBarrier{entered: make(chan context.Context, 256), release: make(chan struct{})}
	b.unblock = sync.OnceFunc(func() { close(b.release) })
	t.Cleanup(b.unblock)
	return b
}

func (b *g004AdmissionBarrier) block(ctx context.Context) {
	b.calls.Add(1)
	active := b.active.Add(1)
	defer b.active.Add(-1)
	for peak := b.peak.Load(); active > peak; peak = b.peak.Load() {
		if b.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	b.entered <- ctx
	<-b.release
}

func (b *g004AdmissionBarrier) await(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-b.entered:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("admitted callback did not reach its barrier")
		return nil
	}
}

func g004AdmissionWait(t *testing.T, condition func() bool, message string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !condition() {
		select {
		case <-timer.C:
			t.Fatal(message)
		default:
			runtime.Gosched()
		}
	}
}

func g004AdmissionDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("callback cancellation or reader settlement did not complete")
	}
}

func newG004AdmissionOwner(t *testing.T, h *g004AdmissionHandler, metadata bool, requests, notifications int) *Owner {
	t.Helper()
	var handler muxcore.SessionHandler = h
	if metadata {
		handler = &g004AdmissionMetaHandler{h}
	}
	o, err := NewOwner(OwnerConfig{
		IPCPath:                    testIPCPath(t),
		ServerID:                   "g004-admission",
		SessionHandler:             handler,
		MaxConcurrentRequests:      requests,
		MaxConcurrentNotifications: notifications,
		MaintenanceGate:            &sync.RWMutex{},
		OnFrameReceived: func(_ string, _ int, _ string) muxcore.FrameAction {
			h.frames.Add(1)
			return muxcore.FramePass
		},
		Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	t.Cleanup(o.Shutdown)
	return o
}

func newG004AdmissionSession(t *testing.T, o *Owner) (*Session, *safeBuf) {
	t.Helper()
	reader, writer := io.Pipe()
	output := &safeBuf{}
	s := NewSession(reader, output)
	s.Cwd = "/g004/shared-project"
	o.AddSession(s)
	t.Cleanup(func() { _ = writer.Close(); s.Close() })
	return s, output
}

func g004AdmissionDispatch(t *testing.T, o *Owner, s *Session, raw string) {
	t.Helper()
	if err := o.handleDownstreamMessage(s, parseMessage([]byte(raw))); err != nil {
		t.Fatalf("handleDownstreamMessage: %v", err)
	}
}

func g004AdmissionRequest(index int) (string, string) {
	id := fmt.Sprintf("%d", index)
	if index%2 != 0 {
		id = fmt.Sprintf(`"request-%d"`, index)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"g004/block"}`, id), id
}

func g004AdmissionResponse(raw []byte) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, parseMessage(raw).ID))
}

func g004AdmissionAssertPaths(t *testing.T, h *g004AdmissionHandler, metadata bool, calls int64) {
	t.Helper()
	plain, meta := calls, int64(0)
	if metadata {
		plain, meta = 0, calls
	}
	if h.plainCalls.Load() != plain || h.metaCalls.Load() != meta {
		t.Fatalf("callback paths: plain=%d metadata=%d, want %d/%d", h.plainCalls.Load(), h.metaCalls.Load(), plain, meta)
	}
}

func TestG004AdmissionRequestsBoundedAcrossSessions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata bool
		limit    int
		burst    int
	}{
		{"plain", false, 2, 6},
		{"metadata", true, 2, 6},
		{"64_of_128", false, 64, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &g004AdmissionHandler{}
			o := newG004AdmissionOwner(t, h, tc.metadata, tc.limit, 1)
			s1, out1 := newG004AdmissionSession(t, o)
			s2, out2 := newG004AdmissionSession(t, o)
			barrier := newG004AdmissionBarrier(t)
			h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
				barrier.block(ctx)
				return g004AdmissionResponse(raw), nil
			}
			sessions := []*Session{s1, s2}
			wantRejected := []map[string]bool{{}, {}}
			for i := range tc.burst {
				raw, id := g004AdmissionRequest(i)
				g004AdmissionDispatch(t, o, sessions[i%2], raw)
				if got := o.PendingRequests(); got > int64(tc.limit) {
					t.Fatalf("request %d raised pending to %d above owner cap %d", i, got, tc.limit)
				}
				if i < tc.limit {
					barrier.await(t)
				} else {
					wantRejected[i%2][id] = true
				}
			}
			if o.PendingRequests() != int64(tc.limit) || barrier.calls.Load() != int64(tc.limit) || barrier.peak.Load() != int64(tc.limit) {
				t.Fatalf("blocked admission: pending=%d callbacks=%d peak=%d, want %d", o.PendingRequests(), barrier.calls.Load(), barrier.peak.Load(), tc.limit)
			}
			for i, output := range []*safeBuf{out1, out2} {
				responses := parseJSONRPCResponsesWithID(t, output.String())
				if len(responses) != len(wantRejected[i]) {
					t.Fatalf("session %d overflow responses=%d, want %d: %s", i, len(responses), len(wantRejected[i]), output.String())
				}
				for _, response := range responses {
					id := string(response.ID)
					if !wantRejected[i][id] || response.Error == nil || response.Error.Code != -32004 {
						t.Fatalf("overflow lost original numeric/string ID or -32004: %s", output.String())
					}
					delete(wantRejected[i], id)
				}
			}
			barrier.unblock()
			g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "released request bodies remained pending")
			g004AdmissionAssertPaths(t, h, tc.metadata, int64(tc.limit))
			if h.frames.Load() != int64(tc.burst) {
				t.Fatalf("frame hook observed %d frames, want admitted and overflow %d", h.frames.Load(), tc.burst)
			}
			if got := len(parseJSONRPCResponsesWithID(t, out1.String())) + len(parseJSONRPCResponsesWithID(t, out2.String())); got != tc.burst {
				t.Fatalf("terminal responses=%d, want exactly %d", got, tc.burst)
			}
		})
	}
}

func TestG004AdmissionNotificationsDropAcrossSessions(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata_%t", metadata), func(t *testing.T) {
			h := &g004AdmissionHandler{}
			o := newG004AdmissionOwner(t, h, metadata, 1, 2)
			s1, out1 := newG004AdmissionSession(t, o)
			s2, out2 := newG004AdmissionSession(t, o)
			requests := newG004AdmissionBarrier(t)
			notifications := newG004AdmissionBarrier(t)
			h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
				requests.block(ctx)
				return g004AdmissionResponse(raw), nil
			}
			h.notification = func(ctx context.Context, _ []byte) { notifications.block(ctx) }
			g004AdmissionDispatch(t, o, s1, `{"jsonrpc":"2.0","id":1,"method":"g004/block"}`)
			requests.await(t)
			for i := range 8 {
				s := s1
				if i%2 != 0 {
					s = s2
				}
				g004AdmissionDispatch(t, o, s, `{"jsonrpc":"2.0","method":"notifications/g004"}`)
				if got := o.nativeWork.Load(); got > 2 {
					t.Fatalf("notification %d reserved %d native callbacks above owner cap 2", i, got)
				}
				if i < 2 {
					notifications.await(t)
				}
			}
			if notifications.calls.Load() != 2 || notifications.peak.Load() != 2 || o.nativeWork.Load() != 2 || o.PendingRequests() != 1 {
				t.Fatalf("independent request/notification caps: notifications=%d peak=%d native=%d pending=%d", notifications.calls.Load(), notifications.peak.Load(), o.nativeWork.Load(), o.PendingRequests())
			}
			if out1.String() != "" || out2.String() != "" {
				t.Fatalf("notifications produced a reply while the only request remained blocked: %q / %q", out1.String(), out2.String())
			}
			notifications.unblock()
			requests.unblock()
			g004AdmissionWait(t, func() bool { return o.nativeWork.Load() == 0 && o.PendingRequests() == 0 }, "callbacks did not finish")
			g004AdmissionAssertPaths(t, h, metadata, 3)
			if notifications.calls.Load() != 2 || notifications.peak.Load() != 2 || h.frames.Load() != 9 {
				t.Fatal("dropped notifications were queued, dispatched later, or skipped the frame hook")
			}
			if len(parseJSONRPCResponsesWithID(t, out1.String())) != 1 || out2.String() != "" {
				t.Fatalf("dropped notifications acquired response obligations: %q / %q", out1.String(), out2.String())
			}
		})
	}
}

func TestG004AdmissionRequestSlotReleasedOnReturn(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		for _, outcome := range []string{"completion", "error", "cancellation", "panic"} {
			t.Run(fmt.Sprintf("metadata_%t/%s", metadata, outcome), func(t *testing.T) {
				h := &g004AdmissionHandler{}
				o := newG004AdmissionOwner(t, h, metadata, 1, 1)
				s1, out1 := newG004AdmissionSession(t, o)
				s2, out2 := newG004AdmissionSession(t, o)
				barrier := newG004AdmissionBarrier(t)
				h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
					barrier.block(ctx)
					if string(parseMessage(raw).ID) == "1" {
						switch outcome {
						case "error":
							return nil, errors.New("g004 callback error")
						case "cancellation":
							return nil, ctx.Err()
						case "panic":
							panic("g004 callback panic")
						}
					}
					return g004AdmissionResponse(raw), nil
				}
				g004AdmissionDispatch(t, o, s1, `{"jsonrpc":"2.0","id":1,"method":"g004/block"}`)
				ctx := barrier.await(t)
				if outcome == "cancellation" {
					s1.Close()
					g004AdmissionDone(t, ctx.Done())
				}
				barrier.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "request slot leaked on "+outcome)
				if outcome != "cancellation" {
					response := parseJSONRPCResponseLine(t, out1.String())
					if string(response.ID) != "1" || (outcome == "completion" && response.Error != nil) || (outcome != "completion" && (response.Error == nil || response.Error.Code != -32603)) {
						t.Fatalf("unexpected %s disposition: %s", outcome, out1.String())
					}
				}
				g004AdmissionDispatch(t, o, s2, `{"jsonrpc":"2.0","id":"next","method":"g004/block"}`)
				barrier.await(t)
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "replacement request did not complete")
				response := parseJSONRPCResponseLine(t, out2.String())
				if string(response.ID) != `"next"` || response.Error != nil {
					t.Fatalf("released slot did not admit new work: %s", out2.String())
				}
				g004AdmissionAssertPaths(t, h, metadata, 2)
				select {
				case <-o.Done():
					t.Fatal("callback outcome terminated the owner")
				default:
				}
			})
		}
	}
}

func TestG004AdmissionNotificationSlotReleasedOnReturn(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		for _, outcome := range []string{"completion", "cancellation", "panic"} {
			t.Run(fmt.Sprintf("metadata_%t/%s", metadata, outcome), func(t *testing.T) {
				h := &g004AdmissionHandler{}
				o := newG004AdmissionOwner(t, h, metadata, 1, 1)
				s1, out1 := newG004AdmissionSession(t, o)
				s2, out2 := newG004AdmissionSession(t, o)
				barrier := newG004AdmissionBarrier(t)
				h.notification = func(ctx context.Context, _ []byte) {
					barrier.block(ctx)
					if barrier.calls.Load() == 1 && outcome == "panic" {
						panic("g004 notification panic")
					}
				}
				g004AdmissionDispatch(t, o, s1, `{"jsonrpc":"2.0","method":"notifications/g004"}`)
				ctx := barrier.await(t)
				if outcome == "cancellation" {
					s1.Close()
					g004AdmissionDone(t, ctx.Done())
				}
				barrier.unblock()
				g004AdmissionWait(t, func() bool { return o.nativeWork.Load() == 0 }, "notification slot leaked on "+outcome)
				g004AdmissionDispatch(t, o, s2, `{"jsonrpc":"2.0","method":"notifications/g004"}`)
				barrier.await(t)
				g004AdmissionWait(t, func() bool { return o.nativeWork.Load() == 0 }, "replacement notification did not complete")
				g004AdmissionAssertPaths(t, h, metadata, 2)
				if out1.String() != "" || out2.String() != "" || o.PendingRequests() != 0 {
					t.Fatal("notification completion/panic created a response or pending request")
				}
				select {
				case <-o.Done():
					t.Fatal("notification outcome terminated the owner")
				default:
				}
			})
		}
	}
}

func TestG004AdmissionCancellationRetainsSlotUntilActualReturn(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		for _, notification := range []bool{false, true} {
			t.Run(fmt.Sprintf("metadata_%t/notification_%t", metadata, notification), func(t *testing.T) {
				h := &g004AdmissionHandler{}
				o := newG004AdmissionOwner(t, h, metadata, 1, 1)
				s1, _ := newG004AdmissionSession(t, o)
				s2, out2 := newG004AdmissionSession(t, o)
				barrier := newG004AdmissionBarrier(t)
				h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
					barrier.block(ctx)
					return g004AdmissionResponse(raw), nil
				}
				h.notification = func(ctx context.Context, _ []byte) { barrier.block(ctx) }
				raw := `{"jsonrpc":"2.0","id":1,"method":"g004/block"}`
				if notification {
					raw = `{"jsonrpc":"2.0","method":"notifications/g004"}`
				}
				g004AdmissionDispatch(t, o, s1, raw)
				ctx := barrier.await(t)
				s1.Close()
				g004AdmissionDone(t, ctx.Done())
				g004AdmissionDispatch(t, o, s2, raw)
				if barrier.calls.Load() != 1 {
					t.Fatal("requested cancellation released a running callback's slot")
				}
				if notification {
					if o.nativeWork.Load() != 1 || out2.String() != "" {
						t.Fatal("cancelled notification lost native-work reservation or replied to overflow")
					}
				} else {
					response := parseJSONRPCResponseLine(t, out2.String())
					if o.PendingRequests() != 1 || string(response.ID) != "1" || response.Error == nil || response.Error.Code != -32004 {
						t.Fatalf("cancelled request lost reservation or overflow disposition: pending=%d %s", o.PendingRequests(), out2.String())
					}
				}
				barrier.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 && o.nativeWork.Load() == 0 }, "actual callback return did not release its slot")
				g004AdmissionDispatch(t, o, s2, raw)
				barrier.await(t)
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 && o.nativeWork.Load() == 0 }, "replacement work did not settle")
				g004AdmissionAssertPaths(t, h, metadata, 2)
				if notification {
					if out2.String() != "" {
						t.Fatal("replacement notification generated a response")
					}
				} else {
					responses := parseJSONRPCResponsesWithID(t, out2.String())
					if len(responses) != 2 || responses[1].Error != nil {
						t.Fatalf("slot did not reopen after actual return: %s", out2.String())
					}
				}
			})
		}
	}
}

func TestG004AdmissionMaintenanceWaitsForCancelledBodies(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata_%t", metadata), func(t *testing.T) {
			h := &g004AdmissionHandler{}
			o := newG004AdmissionOwner(t, h, metadata, 1, 1)
			s, _ := newG004AdmissionSession(t, o)
			requests := newG004AdmissionBarrier(t)
			notifications := newG004AdmissionBarrier(t)
			h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
				requests.block(ctx)
				return g004AdmissionResponse(raw), nil
			}
			h.notification = func(ctx context.Context, _ []byte) { notifications.block(ctx) }
			g004AdmissionDispatch(t, o, s, `{"jsonrpc":"2.0","id":1,"method":"g004/block"}`)
			requestCtx := requests.await(t)
			g004AdmissionDispatch(t, o, s, `{"jsonrpc":"2.0","method":"notifications/g004"}`)
			notificationCtx := notifications.await(t)
			if !o.maintenanceGate.TryLock() {
				t.Fatal("blocked user callbacks retained the global admission lease")
			}
			o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: time.Now()})
			o.maintenanceGate.Unlock()
			s.Close()
			g004AdmissionDone(t, requestCtx.Done())
			g004AdmissionDone(t, notificationCtx.Done())
			if o.PendingRequests() != 1 || o.nativeWork.Load() != 1 {
				t.Fatalf("cancellation erased actual work: pending=%d native=%d", o.PendingRequests(), o.nativeWork.Load())
			}
			assertBlocked := func() {
				t.Helper()
				_, finalized, err := o.FinalizeForRemoval(false, time.Second)
				if finalized || err == nil || o.MaintenanceRetired() || o.MaterializationState() != MaterializationFinalizeBlocked {
					t.Fatalf("maintenance escaped running callbacks: finalized=%t err=%v state=%s", finalized, err, o.MaterializationState())
				}
				select {
				case <-o.Done():
					t.Fatal("owner completed before actual callback return")
				default:
				}
			}
			assertBlocked()
			requests.unblock()
			g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "request did not actually return")
			if o.nativeWork.Load() != 1 {
				t.Fatal("request completion released the still-running notification reservation")
			}
			assertBlocked()
			notifications.unblock()
			g004AdmissionWait(t, func() bool { return o.nativeWork.Load() == 0 && o.SessionCount() == 0 }, "notification/reader did not actually settle")
			g004AdmissionDone(t, o.acceptDone)
			_, finalized, err := o.FinalizeForRemoval(false, time.Second)
			if !finalized || err != nil || !o.MaintenanceRetired() {
				t.Fatalf("actual callback settlement did not permit retirement: finalized=%t err=%v", finalized, err)
			}
			g004AdmissionDone(t, o.Done())
			g004AdmissionAssertPaths(t, h, metadata, 2)
		})
	}
}

func TestG004AdmissionNonpositiveLimitsPreserveLegacy(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			h := &g004AdmissionHandler{}
			o := newG004AdmissionOwner(t, h, false, limit, limit)
			s, output := newG004AdmissionSession(t, o)
			barrier := newG004AdmissionBarrier(t)
			h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
				barrier.block(ctx)
				return g004AdmissionResponse(raw), nil
			}
			h.notification = func(ctx context.Context, _ []byte) { barrier.block(ctx) }
			for i := range 3 {
				raw, _ := g004AdmissionRequest(i)
				g004AdmissionDispatch(t, o, s, raw)
				barrier.await(t)
				g004AdmissionDispatch(t, o, s, `{"jsonrpc":"2.0","method":"notifications/g004"}`)
				barrier.await(t)
			}
			if o.PendingRequests() != 3 || o.nativeWork.Load() != 3 || output.String() != "" {
				t.Fatal("nonpositive limits changed unlimited legacy admission")
			}
			barrier.unblock()
			g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 && o.nativeWork.Load() == 0 }, "unlimited callbacks did not settle")
			g004AdmissionAssertPaths(t, h, false, 6)
			if len(parseJSONRPCResponsesWithID(t, output.String())) != 3 {
				t.Fatal("unlimited requests were rejected or notifications produced replies")
			}
		})
	}
}

func TestG004AdmissionConcurrentBurstCannotOverbook(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		for _, notification := range []bool{false, true} {
			t.Run(fmt.Sprintf("metadata_%t/notification_%t", metadata, notification), func(t *testing.T) {
				h := &g004AdmissionHandler{}
				o := newG004AdmissionOwner(t, h, metadata, 2, 2)
				s1, out1 := newG004AdmissionSession(t, o)
				s2, out2 := newG004AdmissionSession(t, o)
				barrier := newG004AdmissionBarrier(t)
				h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
					barrier.block(ctx)
					return g004AdmissionResponse(raw), nil
				}
				h.notification = func(ctx context.Context, _ []byte) { barrier.block(ctx) }
				start := make(chan struct{})
				finished := make(chan struct{})
				dispatchErrors := make(chan error, 32)
				var submissions sync.WaitGroup
				for i := range 32 {
					submissions.Add(1)
					go func() {
						defer submissions.Done()
						raw, _ := g004AdmissionRequest(i)
						if notification {
							raw = `{"jsonrpc":"2.0","method":"notifications/g004"}`
						}
						s := s1
						if i%2 != 0 {
							s = s2
						}
						<-start
						dispatchErrors <- o.handleDownstreamMessage(s, parseMessage([]byte(raw)))
					}()
				}
				go func() { submissions.Wait(); close(finished) }()
				close(start)
				g004AdmissionDone(t, finished)
				for range 32 {
					if err := <-dispatchErrors; err != nil {
						t.Fatalf("concurrent dispatch: %v", err)
					}
				}
				barrier.await(t)
				barrier.await(t)
				if barrier.calls.Load() != 2 || barrier.peak.Load() != 2 {
					t.Fatalf("concurrent burst overbooked callbacks: calls=%d peak=%d", barrier.calls.Load(), barrier.peak.Load())
				}
				if notification {
					if o.nativeWork.Load() != 2 || o.PendingRequests() != 0 || out1.String() != "" || out2.String() != "" {
						t.Fatal("concurrent notifications exceeded the cap or produced replies")
					}
				} else if o.PendingRequests() != 2 || o.nativeWork.Load() != 0 {
					t.Fatalf("concurrent request reservations: pending=%d native=%d", o.PendingRequests(), o.nativeWork.Load())
				}
				barrier.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 && o.nativeWork.Load() == 0 }, "concurrent callbacks did not settle")
				g004AdmissionAssertPaths(t, h, metadata, 2)
				if barrier.peak.Load() != 2 || h.frames.Load() != 32 {
					t.Fatal("concurrent overflow bypassed synchronous admission or the frame hook")
				}
			})
		}
	}
}
