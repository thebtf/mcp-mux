package supervisor

import (
	"fmt"

	"github.com/thebtf/mcp-mux/muxcore/era"
)

func (runner *runner) modern() bool { return runner.cfg.ProtocolEra == era.EraModern20260728 }

func (runner *runner) validateModernHostRequest(frame *parsedFrame) error {
	if frame.id == nil {
		return fmt.Errorf("request id is missing")
	}
	if _, exists := runner.host.requests[frame.id.key]; exists {
		return fmt.Errorf("duplicate active request id")
	}
	if len(runner.host.requests) >= activeCorrelationLimit {
		return fmt.Errorf("active correlation limit reached")
	}
	if runner.pending.containsRequest(frame.id.key) {
		return fmt.Errorf("duplicate pending request id")
	}
	if frame.progressToken != nil {
		if _, exists := runner.host.tokens[frame.progressToken.key]; exists {
			return fmt.Errorf("duplicate active progress token")
		}
		if runner.pending.containsProgressToken(frame.progressToken.key) {
			return fmt.Errorf("duplicate pending progress token")
		}
	}
	return nil
}

func (runner *runner) finishModernRequest(record *requestRecord) {
	delete(runner.host.requests, record.id.key)
	runner.host.removeToken(record)
}

func (runner *runner) handleModernHost(item *pendingFrame) {
	frame := item.frame
	if isResponse(frame) {
		return // Modern child requests are contained, so no host response can belong to them.
	}
	if frame.method == "notifications/cancelled" && frame.kind == frameNotification {
		if frame.cancellation == nil {
			return
		}
		key := frame.cancellation.requestID.key
		if runner.pending.removeModernRequest(key) {
			return
		}
		record := runner.host.requests[key]
		if runner.current == nil || record == nil || record.generation != runner.current.id {
			return
		}
		if runner.state == StateQuiescing {
			runner.enqueueHost(item, true)
			return
		}
		if runner.state != StateActive {
			return
		}
		runner.finishModernRequest(record)
		outcome, err := runner.writeChild(frame.raw)
		if outcome != writeDelivered || err != nil {
			runner.replaceCurrent(ReasonWriteFailure)
		}
		return
	}
	if frame.method == "notifications/progress" && frame.kind == frameNotification {
		return // There is no modern child-to-host request whose progress could flow this way.
	}
	if runner.state == StateActive {
		runner.deliverHostFirst(item)
	} else {
		runner.enqueueHost(item, false)
	}
}

func (queue *pendingQueue) removeModernRequest(key scalarKey) bool {
	for index, item := range queue.items {
		if item.frame.kind != frameRequest || item.frame.id == nil || item.frame.id.key != key {
			continue
		}
		queue.bytes -= len(item.frame.raw) + 1
		copy(queue.items[index:], queue.items[index+1:])
		queue.items[len(queue.items)-1] = nil
		queue.items = queue.items[:len(queue.items)-1]
		return true
	}
	return false
}

func (runner *runner) handleModernChild(frame *parsedFrame) {
	frame.raw = frame.wire
	if runner.state != StateActive && runner.state != StateQuiescing && runner.state != StateAwaitingDormantExit {
		return
	}
	if runner.state == StateAwaitingDormantExit {
		if runner.dormancyInvalid {
			return
		}
		runner.dormancyInvalid = true
	}
	if frame.kind == frameRequest {
		return
	}
	if isResponse(frame) {
		record := runner.host.requests[frame.id.key]
		if record == nil || record.generation != runner.current.id {
			return
		}
		if err := runner.writeHost(frame.raw); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
			return
		}
		runner.finishModernRequest(record)
		return
	}
	if frame.method == "notifications/cancelled" {
		if frame.cancellation == nil {
			return
		}
		record := runner.host.requests[frame.cancellation.requestID.key]
		if record == nil || record.generation != runner.current.id || record.method != "subscriptions/listen" {
			return
		}
		if err := runner.writeHost(frame.raw); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
			return
		}
		runner.finishModernRequest(record)
		return
	}
	if frame.method == "notifications/progress" {
		runner.handleChildProgress(frame)
		return
	}
	key, present, err := modernSubscriptionKey(frame)
	if err != nil {
		return
	}
	if present {
		record := runner.host.requests[key]
		if record == nil || record.generation != runner.current.id || record.method != "subscriptions/listen" {
			return
		}
	}
	if err := runner.writeHost(frame.raw); err != nil {
		runner.terminate(ReasonHostOutputFailure, err)
	}
}

func modernSubscriptionKey(frame *parsedFrame) (scalarKey, bool, error) {
	if len(frame.params) == 0 {
		return scalarKey{}, false, nil
	}
	params, err := decodeObject(frame.params)
	if err != nil {
		return scalarKey{}, false, err
	}
	rawMeta, ok := params["_meta"]
	if !ok {
		return scalarKey{}, false, nil
	}
	meta, err := decodeObject(rawMeta)
	if err != nil {
		return scalarKey{}, false, err
	}
	rawID, ok := meta["io.modelcontextprotocol/subscriptionId"]
	if !ok {
		return scalarKey{}, false, nil
	}
	id, err := parseScalar(rawID)
	return id.key, true, err
}
