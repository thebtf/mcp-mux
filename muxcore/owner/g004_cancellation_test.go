package owner

import (
	"context"
	"fmt"
	"sync"
	"testing"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/era"
)

func newG004CancellationOwner(t *testing.T, h *g004AdmissionHandler, protocol era.ProtocolEra, metadata bool, limit int) *Owner {
	t.Helper()
	var handler muxcore.SessionHandler = h
	if metadata {
		handler = &g004AdmissionMetaHandler{h}
	}
	o, err := NewOwner(OwnerConfig{
		IPCPath:                    testIPCPath(t),
		ServerID:                   "g004-cancellation",
		SessionHandler:             handler,
		ProtocolEra:                protocol,
		ModernShared:               protocol == era.EraModern20260728,
		MaxConcurrentRequests:      limit,
		MaxConcurrentNotifications: 1,
		MaintenanceGate:            &sync.RWMutex{},
		Logger:                     testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	t.Cleanup(o.Shutdown)
	return o
}

func g004CancellationRequest(protocol era.ProtocolEra, id, method string) string {
	params := `{}`
	if protocol == era.EraModern20260728 {
		params = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, id, method, params)
}

func g004CancellationNotify(o *Owner, s *Session, id string) {
	// Local context cancellation must not depend on an upstream transport being configured.
	_ = o.handleDownstreamMessage(s, parseMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s}}`, id))))
}

func g004CancellationAssertLive(t *testing.T, contexts ...context.Context) {
	t.Helper()
	for index, ctx := range contexts {
		if ctx.Err() != nil {
			t.Fatalf("unmatched cancellation reached callback %d: %v", index, ctx.Err())
		}
	}
}

func TestG004CancellationMatchesSessionAndTypedID(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		for _, metadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("era_%d/metadata_%t", protocol, metadata), func(t *testing.T) {
				h := &g004AdmissionHandler{}
				o := newG004CancellationOwner(t, h, protocol, metadata, 4)
				s, output := newG004AdmissionSession(t, o)
				sibling, siblingOutput := newG004AdmissionSession(t, o)
				initial := newG004AdmissionBarrier(t)
				reused := newG004AdmissionBarrier(t)
				h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
					if parseMessage(raw).Method == "g004/reused" {
						reused.block(ctx)
					} else {
						initial.block(ctx)
					}
					return g004AdmissionResponse(raw), nil
				}

				g004AdmissionDispatch(t, o, s, g004CancellationRequest(protocol, "1", "g004/block"))
				numeric := initial.await(t)
				g004AdmissionDispatch(t, o, s, g004CancellationRequest(protocol, `"1"`, "g004/block"))
				text := initial.await(t)
				g004AdmissionDispatch(t, o, sibling, g004CancellationRequest(protocol, "1", "g004/block"))
				siblingNumeric := initial.await(t)

				g004CancellationNotify(o, s, `"unknown"`)
				g004CancellationAssertLive(t, numeric, text, siblingNumeric)
				g004CancellationNotify(o, sibling, `"1"`)
				g004CancellationAssertLive(t, numeric, text, siblingNumeric)

				g004CancellationNotify(o, s, "1")
				g004AdmissionDone(t, numeric.Done())
				g004CancellationAssertLive(t, text, siblingNumeric)
				g004CancellationNotify(o, s, "1")
				g004CancellationNotify(o, s, "1")
				g004CancellationAssertLive(t, text, siblingNumeric)
				if o.PendingRequests() != 3 || output.String() != "" || siblingOutput.String() != "" {
					t.Fatal("cancellation settled a blocked callback or generated a notification response")
				}

				g004CancellationNotify(o, s, `"1"`)
				g004AdmissionDone(t, text.Done())
				g004CancellationAssertLive(t, siblingNumeric)
				initial.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "initial callback bodies did not return")
				responses := parseJSONRPCResponsesWithID(t, output.String())
				if len(responses) != 2 {
					t.Fatalf("initial requests produced %d terminal responses, want 2", len(responses))
				}
				seen := map[string]bool{}
				for _, response := range responses {
					seen[string(response.ID)] = true
				}
				if !seen["1"] || !seen[`"1"`] || len(parseJSONRPCResponsesWithID(t, siblingOutput.String())) != 1 {
					t.Fatal("cancellation changed typed response IDs or affected the sibling response")
				}

				g004CancellationNotify(o, s, "1")
				g004CancellationNotify(o, s, `"1"`)
				g004AdmissionDispatch(t, o, s, g004CancellationRequest(protocol, "1", "g004/reused"))
				next := reused.await(t)
				g004CancellationAssertLive(t, next)
				g004CancellationNotify(o, s, `"1"`)
				g004CancellationAssertLive(t, next)
				g004CancellationNotify(o, s, "1")
				g004AdmissionDone(t, next.Done())
				if o.PendingRequests() != 1 {
					t.Fatal("reused request ID lost its reservation before the new callback returned")
				}
				reused.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "reused request callback did not return")
				responses = parseJSONRPCResponsesWithID(t, output.String())
				if len(responses) != 3 || string(responses[2].ID) != "1" || len(parseJSONRPCResponsesWithID(t, siblingOutput.String())) != 1 {
					t.Fatal("ID reuse or repeated cancellation changed the terminal response obligations")
				}
				g004AdmissionAssertPaths(t, h, metadata, 4)
			})
		}
	}
}

func TestG004CancellationRetains64SlotsUntilActualReturn(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		for _, metadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("era_%d/metadata_%t", protocol, metadata), func(t *testing.T) {
				h := &g004AdmissionHandler{}
				o := newG004CancellationOwner(t, h, protocol, metadata, 64)
				s, output := newG004AdmissionSession(t, o)
				sibling, siblingOutput := newG004AdmissionSession(t, o)
				first := newG004AdmissionBarrier(t)
				others := newG004AdmissionBarrier(t)
				readmitted := newG004AdmissionBarrier(t)
				h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
					switch string(parseMessage(raw).ID) {
					case "0":
						first.block(ctx)
					case `"after-return"`:
						readmitted.block(ctx)
					default:
						others.block(ctx)
					}
					return g004AdmissionResponse(raw), nil
				}

				contexts := make([]context.Context, 64)
				ids := make([]string, 64)
				for index := range contexts {
					_, ids[index] = g004AdmissionRequest(index)
					g004AdmissionDispatch(t, o, s, g004CancellationRequest(protocol, ids[index], "g004/block"))
					if index == 0 {
						contexts[index] = first.await(t)
					} else {
						contexts[index] = others.await(t)
					}
				}
				for _, id := range ids {
					g004CancellationNotify(o, s, id)
				}
				for _, ctx := range contexts {
					g004AdmissionDone(t, ctx.Done())
				}
				if o.PendingRequests() != 64 || output.String() != "" {
					t.Fatal("cancelled contexts released capacity or completed responses while callback bodies were blocked")
				}
				g004AdmissionAssertPaths(t, h, metadata, 64)

				g004AdmissionDispatch(t, o, sibling, g004CancellationRequest(protocol, `"before-return"`, "g004/block"))
				responses := parseJSONRPCResponsesWithID(t, siblingOutput.String())
				if len(responses) != 1 || string(responses[0].ID) != `"before-return"` || responses[0].Error == nil || responses[0].Error.Code != -32004 {
					t.Fatalf("sibling was not rate-limited while all 64 callback bodies remained active: %s", siblingOutput.String())
				}
				if o.PendingRequests() != 64 {
					t.Fatal("sibling rejection changed the 64 active callback reservations")
				}
				g004AdmissionAssertPaths(t, h, metadata, 64)

				first.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 63 }, "one actual callback return did not release exactly one slot")
				g004AdmissionDispatch(t, o, sibling, g004CancellationRequest(protocol, `"after-return"`, "g004/block"))
				next := readmitted.await(t)
				g004CancellationAssertLive(t, next)
				if o.PendingRequests() != 64 || len(parseJSONRPCResponsesWithID(t, output.String())) != 1 || len(parseJSONRPCResponsesWithID(t, siblingOutput.String())) != 1 {
					t.Fatal("sibling admission did not consume only the slot released by the actual callback return")
				}
				g004AdmissionAssertPaths(t, h, metadata, 65)

				others.unblock()
				readmitted.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "released callback bodies did not settle")
				responses = parseJSONRPCResponsesWithID(t, siblingOutput.String())
				if len(parseJSONRPCResponsesWithID(t, output.String())) != 64 || len(responses) != 2 || string(responses[1].ID) != `"after-return"` || responses[1].Error != nil || responses[1].Result == nil {
					t.Fatal("actual callback returns did not complete the original requests and readmitted sibling exactly once")
				}
			})
		}
	}
}

func TestG004CancellationDuplicateIDDoesNotReplaceActiveCallback(t *testing.T) {
	for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
		for _, olderReturnsFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("era_%d/older_returns_first_%t", protocol, olderReturnsFirst), func(t *testing.T) {
				h := &g004AdmissionHandler{}
				o := newG004CancellationOwner(t, h, protocol, true, 2)
				s, output := newG004AdmissionSession(t, o)
				first := newG004AdmissionBarrier(t)
				second := newG004AdmissionBarrier(t)
				h.request = func(ctx context.Context, raw []byte) ([]byte, error) {
					if parseMessage(raw).Method == "g004/first" {
						first.block(ctx)
					} else {
						second.block(ctx)
					}
					return g004AdmissionResponse(raw), nil
				}

				g004AdmissionDispatch(t, o, s, g004CancellationRequest(protocol, "1", "g004/first"))
				firstCtx := first.await(t)
				g004AdmissionDispatch(t, o, s, g004CancellationRequest(protocol, "1", "g004/second"))
				secondCtx := second.await(t)
				g004CancellationNotify(o, s, "1")
				g004CancellationAssertLive(t, firstCtx, secondCtx)
				if o.PendingRequests() != 2 || output.String() != "" {
					t.Fatal("ambiguous cancellation changed duplicate request admission or settlement")
				}

				returning, remaining, remainingCtx := first, second, secondCtx
				if !olderReturnsFirst {
					returning, remaining, remainingCtx = second, first, firstCtx
				}
				returning.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 1 }, "returning duplicate callback did not settle")
				g004CancellationAssertLive(t, remainingCtx)
				g004CancellationNotify(o, s, "1")
				g004AdmissionDone(t, remainingCtx.Done())
				if o.PendingRequests() != 1 || len(parseJSONRPCResponsesWithID(t, output.String())) != 1 {
					t.Fatal("cancellation released the remaining duplicate callback before its actual return")
				}
				remaining.unblock()
				g004AdmissionWait(t, func() bool { return o.PendingRequests() == 0 }, "remaining duplicate callback did not settle")
				responses := parseJSONRPCResponsesWithID(t, output.String())
				if len(responses) != 2 || string(responses[0].ID) != "1" || string(responses[1].ID) != "1" {
					t.Fatal("duplicate-ID bookkeeping changed the existing response obligations")
				}
				g004AdmissionAssertPaths(t, h, true, 2)
			})
		}
	}
}
