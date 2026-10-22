package owner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
)

type g004ReviewReader struct {
	*io.PipeReader
	started chan struct{}
	once    sync.Once
}

func (r *g004ReviewReader) Read(data []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.PipeReader.Read(data)
}

func TestG004ReviewLegacyRootsCompleteBeforeReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		released := make(chan struct{})
		release := sync.OnceFunc(func() { close(released) })
		roots := &gatedWriter{started: make(chan struct{}), release: released}
		h := &g004AdmissionHandler{request: func(_ context.Context, raw []byte) ([]byte, error) {
			return g004AdmissionResponse(raw), nil
		}}
		o := newDispatchOwner(h)
		o.upstreamWriter = roots
		reader, input := io.Pipe()
		observed := &g004ReviewReader{PipeReader: reader, started: make(chan struct{})}
		output := &safeBuf{}
		s := NewSession(observed, output)
		s.Cwd = "/g004/review-roots"
		added := make(chan struct{})
		defer func() {
			release()
			_ = input.Close()
			s.Close()
			<-added
			synctest.Wait()
			o.sessionReaders.Wait()
			o.Shutdown()
		}()

		go func() { o.AddSession(s); close(added) }()
		g004LifecycleReceive(t, roots.started, "blocked roots notification write")
		written := make(chan error, 1)
		go func() {
			_, err := fmt.Fprintln(input, `{"jsonrpc":"2.0","id":801,"method":"g004/review"}`)
			written <- err
		}()
		synctest.Wait()
		select {
		case <-observed.started:
			t.Error("legacy session reader began before the roots notification write completed")
		default:
		}
		if h.plainCalls.Load() != 0 || len(written) != 0 || output.String() != "" {
			t.Error("first downstream frame was consumed or dispatched while the roots notification write was blocked")
		}

		release()
		g004LifecycleReceive(t, added, "session admission return")
		g004LifecycleReceive(t, observed.started, "reader start after roots notification")
		if err := g004LifecycleReceive(t, written, "first downstream frame write"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		notification := parseMessage([]byte(roots.String()))
		if !notification.IsNotification() || notification.Method != "notifications/roots/list_changed" {
			t.Fatalf("blocked upstream write was not the roots notification: %s", roots.String())
		}
		response := parseJSONRPCResponseLine(t, output.String())
		if string(response.ID) != "801" || response.Error != nil || response.Result == nil || h.plainCalls.Load() != 1 || o.PendingRequests() != 0 {
			t.Fatalf("released reader did not complete its first request exactly once: %s", output.String())
		}
	})
}

func TestG004ReviewRequestLimitIncludesResponseWrite(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata_%t", metadata), func(t *testing.T) {
			h := &g004AdmissionHandler{request: func(_ context.Context, raw []byte) ([]byte, error) {
				return g004AdmissionResponse(raw), nil
			}}
			o := newG004AdmissionOwner(t, h, metadata, 1, 1)
			released := make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			blocked := &gatedWriter{started: make(chan struct{}), release: released}
			reader, input := io.Pipe()
			slow := NewSession(reader, blocked)
			slow.Cwd = "/g004/shared-project"
			o.AddSession(slow)
			sibling, output := newG004AdmissionSession(t, o)
			t.Cleanup(func() {
				release()
				_ = input.Close()
				slow.Close()
				sibling.Close()
				o.sessionReaders.Wait()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "released response writer did not settle")
			})

			g004AdmissionDispatch(t, o, slow, `{"jsonrpc":"2.0","id":"slow","method":"g004/review"}`)
			g004LifecycleReceive(t, blocked.started, "response write after immediate callback return")
			for index, id := range []string{"802", `"overflow"`} {
				g004AdmissionDispatch(t, o, sibling, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"g004/review"}`, id))
				if pending := o.PendingRequests(); pending != 1 {
					t.Fatalf("blocked response write did not retain the owner cap: pending=%d, want 1", pending)
				}
				g004AdmissionAssertPaths(t, h, metadata, 1)
				responses := parseJSONRPCResponsesWithID(t, output.String())
				if len(responses) != index+1 {
					t.Fatalf("sibling overflow responses=%d, want %d: %s", len(responses), index+1, output.String())
				}
				response := responses[index]
				if string(response.ID) != id || response.Error == nil || response.Error.Code != -32004 {
					t.Fatalf("sibling overflow lost its original ID or -32004: %s", output.String())
				}
			}
			if blocked.String() != "" {
				t.Fatal("gated response write completed before release")
			}

			release()
			g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "completed response write did not release capacity")
			response := parseJSONRPCResponseLine(t, blocked.String())
			if string(response.ID) != `"slow"` || response.Error != nil || response.Result == nil {
				t.Fatalf("slow session did not receive its real response: %s", blocked.String())
			}
			g004AdmissionDispatch(t, o, sibling, `{"jsonrpc":"2.0","id":"readmitted","method":"g004/review"}`)
			g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "readmitted request did not complete")
			responses := parseJSONRPCResponsesWithID(t, output.String())
			if len(responses) != 3 || string(responses[2].ID) != `"readmitted"` || responses[2].Error != nil || responses[2].Result == nil {
				t.Fatalf("capacity was not readmitted after the real response write: %s", output.String())
			}
			g004AdmissionAssertPaths(t, h, metadata, 2)
		})
	}
}

// Expose only legacy interfaces while reusing the metadata lifecycle event fixture.
type g004ReviewLegacyLifecycle struct {
	handler *g004MetadataLifecycleHandler
}

func (h *g004ReviewLegacyLifecycle) OnProjectConnect(project muxcore.ProjectContext) {
	h.handler.OnProjectConnectWithSessionMeta(project, muxcore.SessionMeta{})
}

func (h *g004ReviewLegacyLifecycle) OnProjectDisconnect(projectID string) {
	h.handler.OnProjectDisconnectWithSessionMeta(projectID, muxcore.SessionMeta{})
}

func (h *g004ReviewLegacyLifecycle) HandleRequest(ctx context.Context, project muxcore.ProjectContext, raw []byte) ([]byte, error) {
	return h.handler.HandleRequestWithSessionMeta(ctx, project, muxcore.SessionMeta{}, raw)
}

func TestG004ReviewConnectPanicFailsOnlyItsSession(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata_%t", metadata), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				released := make(chan struct{})
				release := sync.OnceFunc(func() { close(released) })
				h := &g004MetadataLifecycleHandler{next: make(chan g004LifecycleEvent, 64)}
				h.onConnect = func(project muxcore.ProjectContext, meta muxcore.SessionMeta) {
					if project.Env["connection"] == "panic" {
						h.record("connect-panicking", project, meta)
						<-released
						panic("g004 review partial connect")
					}
				}
				var handler muxcore.SessionHandler = h
				if !metadata {
					handler = &g004ReviewLegacyLifecycle{handler: h}
				}
				o := newDispatchOwner(handler)
				firstReader, firstInput := io.Pipe()
				observed := &g004ReviewReader{PipeReader: firstReader, started: make(chan struct{})}
				firstOutput := &safeBuf{}
				first := NewSession(observed, firstOutput)
				first.Cwd = "/g004/review-panic"
				first.Env = map[string]string{"connection": "panic"}
				secondReader, secondInput := io.Pipe()
				secondOutput := &safeBuf{}
				second := NewSession(secondReader, secondOutput)
				second.Cwd = first.Cwd
				second.Env = map[string]string{"connection": "sibling"}
				added := make(chan struct{})
				defer func() {
					release()
					_ = firstInput.Close()
					_ = secondInput.Close()
					first.Close()
					second.Close()
					<-added
					synctest.Wait()
					o.sessionReaders.Wait()
					o.Shutdown()
				}()

				go func() { o.AddSession(first); close(added) }()
				connecting := g004LifecycleExpect(t, h, "connect-panicking")
				g004LifecycleReceive(t, added, "admission while connect is blocked")
				firstMeta, secondMeta := muxcore.SessionMeta{}, muxcore.SessionMeta{}
				if metadata {
					firstMeta, secondMeta = first.Meta(), second.Meta()
				}
				projectID := muxcore.ProjectContextID(first.Cwd)
				g004LifecycleAssertMeta(t, connecting, projectID, firstMeta)
				if first.Meta().SessionID == "" || first.Meta().SessionID == second.Meta().SessionID {
					t.Fatal("connect did not have a distinct native session identity before panicking")
				}
				firstWritten := make(chan error, 1)
				go func() {
					_, err := fmt.Fprintln(firstInput, `{"jsonrpc":"2.0","id":803,"method":"tools/list"}`)
					firstWritten <- err
				}()
				o.AddSession(second)
				g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "connect"), projectID, secondMeta)
				synctest.Wait()
				if len(h.next) != 0 || len(firstWritten) != 0 {
					t.Fatal("request dispatch or disconnect overtook the blocked connect callback")
				}

				release()
				g004LifecycleReceive(t, first.Done(), "panicking session closure")
				g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "disconnect"), projectID, firstMeta)
				if err := g004LifecycleReceive(t, firstWritten, "unconsumed request write failure"); err == nil {
					t.Error("panicking session consumed a request frame")
				}
				synctest.Wait()
				select {
				case <-observed.started:
					t.Error("session reader consumed input after connect panicked")
				default:
				}
				if o.SessionCount() != 1 || second.IsClosed() || firstOutput.String() != "" || len(h.next) != 0 || o.nativeWork.Load() != 0 || o.PendingRequests() != 0 {
					t.Fatal("connect panic left unfinished cleanup, dispatched a request, or affected its sibling")
				}
				first.Close()
				synctest.Wait()
				if len(h.next) != 0 {
					t.Fatal("repeated close repeated the panicking session's disconnect cleanup")
				}

				secondWritten := make(chan error, 1)
				go func() {
					_, err := fmt.Fprintln(secondInput, `{"jsonrpc":"2.0","id":804,"method":"tools/list"}`)
					secondWritten <- err
				}()
				if err := g004LifecycleReceive(t, secondWritten, "same-project sibling request write"); err != nil {
					t.Fatal(err)
				}
				g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "request"), projectID, secondMeta)
				synctest.Wait()
				var response struct {
					ID     int  `json:"id"`
					Result bool `json:"result"`
				}
				if err := json.Unmarshal([]byte(secondOutput.String()), &response); err != nil || response.ID != 804 || !response.Result {
					t.Fatalf("same-project sibling did not survive the connect panic: %s (decode: %v)", secondOutput.String(), err)
				}
				_ = secondInput.Close()
				second.Close()
				g004LifecycleAssertMeta(t, g004LifecycleExpect(t, h, "disconnect"), projectID, secondMeta)
				synctest.Wait()
				o.sessionReaders.Wait()
				if o.SessionCount() != 0 || o.nativeWork.Load() != 0 || o.PendingRequests() != 0 || len(h.next) != 0 {
					t.Fatal("connect panic lifecycle did not settle exactly once per session")
				}
			})
		})
	}
}
