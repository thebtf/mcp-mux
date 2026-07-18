package supervisor

import (
	"errors"
	"fmt"
)

func (runner *runner) handleHostEvent(event hostFrameEvent) {
	if runner.terminal {
		return
	}
	runner.lastHostSequence = event.sequence
	defer runner.resumeDormantLeaseAfterHost()
	if event.err != nil {
		if err := runner.writeHost(localErrorFrame(nil, codeInvalidRequest, "invalid JSON-RPC frame")); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
		}
		return
	}
	item := &pendingFrame{sequence: event.sequence, arrived: event.arrived, frame: event.frame}
	frame := item.frame

	if frame.reserved {
		if frame.kind == frameRequest {
			runner.rejectHostRequest(frame, codeMethodNotFound, "method not found")
		}
		return
	}
	if runner.handleHostLifecycle(item) {
		return
	}

	switch {
	case isResponse(frame):
		if runner.state == StateActive || runner.state == StateReplaying {
			runner.handleHostResponse(frame)
		} else if runner.state == StateQuiescing {
			runner.enqueueHost(item, true)
		}
	case frame.method == "notifications/cancelled":
		runner.handleHostCancellation(item)
	case frame.method == "notifications/progress":
		if runner.state == StateActive {
			runner.handleHostProgress(frame)
		} else if runner.state == StateQuiescing {
			runner.enqueueHost(item, true)
		}
	case frame.method == "notifications/tasks/status":
		if runner.state == StateActive {
			runner.handleHostTaskStatus(frame)
		} else if runner.state == StateQuiescing {
			runner.enqueueHost(item, true)
		}
	case runner.state == StateActive || (runner.state == StateReplaying && isInitializationPing(frame)):
		runner.deliverHostFirst(item)
	default:
		runner.enqueueHost(item, false)
	}
}

func isInitializationPing(frame *parsedFrame) bool {
	return frame != nil && frame.kind == frameRequest && frame.method == "ping"
}

func isInitializationLogging(frame *parsedFrame) bool {
	return frame != nil && frame.kind == frameNotification && frame.method == "notifications/message"
}

func (runner *runner) routeInitializationHostPing(item *pendingFrame) {
	if runner.state == StateActive || runner.state == StateReplaying {
		runner.deliverHostFirst(item)
		return
	}
	runner.enqueueHost(item, false)
}

func (runner *runner) handleHostLifecycle(item *pendingFrame) bool {
	frame := item.frame
	if runner.initializeRequest == nil {
		if frame.kind != frameRequest || frame.method != "initialize" {
			if frame.kind == frameRequest {
				runner.rejectHostRequest(frame, codeInvalidRequest, "initialize must be the first request")
			}
			return true
		}
		if frame.utilityErr != nil {
			runner.rejectHostRequest(frame, codeInvalidParams, "invalid initialize metadata")
			return true
		}
		runner.initializeRequest = append([]byte(nil), frame.raw...)
		id := *frame.id
		runner.initializeID = &id
		runner.initializeAwaiting = true
		if runner.current != nil && runner.state == StateActive {
			runner.writeInitialInitialize(frame)
		} else if runner.current == nil {
			runner.stopTimer(&runner.dormantLeaseTimer)
			runner.beginStart()
		}
		return true
	}

	if runner.initializeAwaiting {
		switch {
		case isResponse(frame):
			return false
		case isInitializationPing(frame):
			runner.routeInitializationHostPing(item)
		case frame.kind == frameRequest:
			runner.rejectHostRequest(frame, codeInvalidRequest, "initialize response is still pending")
		}
		return true
	}
	if runner.initializeFailed {
		if frame.kind == frameRequest {
			runner.rejectHostRequest(frame, codeInvalidRequest, "initialization failed")
		}
		return true
	}
	if runner.initialized == nil {
		if frame.kind == frameNotification && frame.method == "notifications/initialized" {
			runner.initialized = append([]byte(nil), frame.raw...)
			if runner.current != nil && runner.state == StateActive {
				runner.writeLifecycleNotification(frame.raw)
			}
			return true
		}
		switch {
		case isResponse(frame):
			return false
		case isInitializationPing(frame):
			runner.routeInitializationHostPing(item)
		case frame.kind == frameRequest:
			runner.rejectHostRequest(frame, codeInvalidRequest, "notifications/initialized is required")
		}
		return true
	}
	if frame.kind == frameNotification && frame.method == "notifications/initialized" {
		return true
	}
	if frame.kind == frameRequest && frame.method == "initialize" {
		runner.rejectHostRequest(frame, codeInvalidRequest, "initialize already completed")
		return true
	}
	return false
}

func (runner *runner) writeInitialInitialize(frame *parsedFrame) {
	outcome, err := runner.writeChild(frame.raw)
	if outcome == writeDelivered && err == nil {
		runner.replayTimer = runner.cfg.runtimeClock.NewTimer(runner.cfg.ReplayTimeout)
		return
	}
	if outcome == writeNotWritten {
		runner.replaceCurrent(ReasonWriteFailure)
		return
	}
	runner.replaceCurrent(ReasonWriteFailure)
}

func (runner *runner) writeLifecycleNotification(raw []byte) {
	outcome, err := runner.writeChild(raw)
	if outcome != writeDelivered || err != nil {
		runner.replaceCurrent(ReasonWriteFailure)
	}
}

func (runner *runner) enqueueHost(item *pendingFrame, sameGenerationOnly bool) {
	frame := item.frame
	if frame.kind == frameRequest {
		if err := runner.validateHostRequest(frame); err != nil {
			runner.rejectHostRequest(frame, codeInvalidRequest, err.Error())
			return
		}
	}
	item.sameGenerationOnly = sameGenerationOnly
	if !runner.pending.enqueue(item, runner.cfg.PendingFrameLimit, runner.cfg.PendingByteLimit) {
		if frame.kind == frameRequest {
			runner.rejectHostRequest(frame, codePendingFull, "supervisor pending queue is full")
		}
		return
	}
	if runner.state == StateDormant {
		runner.stopTimer(&runner.dormantLeaseTimer)
		runner.beginStart()
	} else if runner.current == nil && !runner.starting {
		runner.beginStart()
	}
}

func (runner *runner) validateHostRequest(frame *parsedFrame) error {
	if frame.utilityErr != nil {
		return fmt.Errorf("invalid request metadata")
	}
	if err := runner.host.canRegister(frame); err != nil {
		return err
	}
	if runner.initializeID != nil &&
		(runner.initializeAwaiting || runner.state == StateReplaying) &&
		frame.id.key == runner.initializeID.key {
		return fmt.Errorf("duplicate active request id")
	}
	if runner.pending.containsRequest(frame.id.key) {
		return fmt.Errorf("duplicate pending request id")
	}
	if frame.progressToken != nil && runner.pending.containsProgressToken(frame.progressToken.key) {
		return fmt.Errorf("duplicate pending progress token")
	}
	if frame.taskOperation != "" {
		task := runner.host.tasks[frame.taskID]
		if task == nil || runner.current == nil || task.generation != runner.current.id {
			return fmt.Errorf("unknown taskId")
		}
	}
	return nil
}

func (runner *runner) deliverHostFirst(item *pendingFrame) {
	frame := item.frame
	canDeliver := runner.state == StateActive || (runner.state == StateReplaying && isInitializationPing(frame))
	if runner.current == nil || !canDeliver {
		runner.enqueueHost(item, false)
		return
	}
	if frame.kind == frameRequest {
		if err := runner.validateHostRequest(frame); err != nil {
			runner.rejectHostRequest(frame, codeInvalidRequest, err.Error())
			return
		}
	}

	outcome, err := runner.writeChild(frame.raw)
	if frame.kind == frameRequest && outcome != writeNotWritten {
		runner.host.register(frame, runner.current.id)
	}
	if outcome == writeDelivered && err == nil {
		return
	}
	if outcome == writeNotWritten {
		runner.retainAfterNotWritten(item)
	}
	runner.replaceCurrent(ReasonWriteFailure)
}

func (runner *runner) retainAfterNotWritten(item *pendingFrame) {
	frame := item.frame
	if frame.kind != frameRequest && frame.kind != frameNotification {
		return
	}
	if !runner.pending.pushFrontBounded(item, runner.cfg.PendingFrameLimit, runner.cfg.PendingByteLimit) && frame.kind == frameRequest {
		runner.rejectHostRequest(frame, codePendingFull, "supervisor pending queue is full")
	}
}

func (runner *runner) handleHostResponse(frame *parsedFrame) {
	if frame.id == nil || runner.current == nil {
		return
	}
	record := runner.child.requests[frame.id.key]
	if record == nil || record.generation != runner.current.id {
		return
	}
	if err := runner.child.canComplete(record, frame); err != nil {
		runner.child.cancel(record)
		outcome, writeErr := runner.writeChild(localErrorFrame(frame.id.raw, codeInvalidParams, err.Error()))
		if outcome != writeDelivered || writeErr != nil {
			runner.replaceCurrent(ReasonWriteFailure)
		}
		return
	}
	outcome, err := runner.writeChild(frame.raw)
	if outcome != writeNotWritten {
		runner.child.complete(record, frame)
	}
	if outcome != writeDelivered || err != nil {
		runner.replaceCurrent(ReasonWriteFailure)
	}
}

func (runner *runner) handleHostCancellation(item *pendingFrame) {
	frame := item.frame
	if frame.utilityErr != nil || frame.cancellation == nil {
		return
	}
	target := frame.cancellation.requestID.key
	if runner.pending.removeRequest(target) {
		return
	}
	record := runner.host.requests[target]
	if record == nil || runner.current == nil || record.generation != runner.current.id || record.initialize || record.taskAugmented {
		return
	}
	if runner.state == StateQuiescing {
		runner.enqueueHost(item, true)
		return
	}
	if runner.state != StateActive {
		return
	}
	runner.host.cancel(record)
	outcome, err := runner.writeChild(frame.raw)
	if outcome != writeDelivered || err != nil {
		runner.replaceCurrent(ReasonWriteFailure)
	}
}

func (runner *runner) handleHostProgress(frame *parsedFrame) {
	if frame.utilityErr != nil || frame.progress == nil || runner.current == nil {
		return
	}
	if !runner.child.acceptProgress(frame.progress, runner.current.id) {
		return
	}
	outcome, err := runner.writeChild(frame.raw)
	if outcome != writeDelivered || err != nil {
		runner.replaceCurrent(ReasonWriteFailure)
	}
}

func (runner *runner) handleHostTaskStatus(frame *parsedFrame) {
	if frame.utilityErr != nil || frame.taskStatus == nil || runner.current == nil {
		return
	}
	if !runner.child.updateTask(frame.taskStatus, runner.current.id) {
		return
	}
	outcome, err := runner.writeChild(frame.raw)
	if outcome != writeDelivered || err != nil {
		runner.replaceCurrent(ReasonWriteFailure)
	}
}

func (runner *runner) handleChildFrame(frame *parsedFrame) {
	if runner.current == nil {
		return
	}
	if frame.reserved {
		runner.handleChildControl(frame)
		return
	}
	if runner.state == StateReplaying {
		runner.handleReplayFrame(frame)
		return
	}
	if runner.state == StateAwaitingDormantExit {
		if runner.dormancyInvalid {
			return
		}
		if err := runner.writeHost(frame.raw); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
			return
		}
		if isResponse(frame) && frame.id != nil {
			if record := runner.host.requests[frame.id.key]; record != nil && record.generation == runner.current.id {
				runner.host.complete(record, frame)
			}
		}
		runner.dormancyInvalid = true
		return
	}
	if runner.state != StateActive && runner.state != StateQuiescing {
		return
	}
	if runner.initialized == nil {
		switch {
		case isResponse(frame):
			runner.handleChildResponse(frame)
		case runner.initializeRequest != nil && isInitializationPing(frame):
			runner.handleChildRequest(frame)
		case runner.initializeRequest != nil && isInitializationLogging(frame):
			if err := runner.writeHost(frame.raw); err != nil {
				runner.terminate(ReasonHostOutputFailure, err)
			}
		case frame.kind == frameRequest:
			runner.replaceCurrent(ReasonProtocolFailure)
		}
		return
	}

	switch {
	case isResponse(frame):
		runner.handleChildResponse(frame)
	case frame.kind == frameRequest:
		runner.handleChildRequest(frame)
	case frame.method == "notifications/cancelled":
		runner.handleChildCancellation(frame)
	case frame.method == "notifications/progress":
		runner.handleChildProgress(frame)
	case frame.method == "notifications/tasks/status":
		runner.handleChildTaskStatus(frame)
	default:
		if err := runner.writeHost(frame.raw); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
		}
	}
}

func (runner *runner) handleChildResponse(frame *parsedFrame) {
	if frame.id == nil || runner.current == nil {
		return
	}
	if runner.initializeAwaiting && runner.initializeID != nil && frame.id.key == runner.initializeID.key {
		runner.completeInitialInitialize(frame)
		return
	}
	record := runner.host.requests[frame.id.key]
	if record == nil || record.generation != runner.current.id {
		return
	}
	if err := runner.host.canComplete(record, frame); err != nil {
		runner.replaceCurrent(ReasonProtocolFailure)
		return
	}
	if err := runner.writeHost(frame.raw); err != nil {
		runner.terminate(ReasonHostOutputFailure, err)
		return
	}
	runner.host.complete(record, frame)
}

func (runner *runner) handleChildRequest(frame *parsedFrame) {
	if runner.current == nil || (runner.initialized == nil && (runner.initializeRequest == nil || !isInitializationPing(frame))) {
		runner.replaceCurrent(ReasonProtocolFailure)
		return
	}
	if frame.utilityErr != nil {
		runner.rejectChildRequest(frame, codeInvalidParams, "invalid request metadata")
		return
	}
	if err := runner.child.canRegister(frame); err != nil {
		runner.replaceCurrent(ReasonProtocolFailure)
		return
	}
	if frame.taskOperation != "" {
		task := runner.child.tasks[frame.taskID]
		if task == nil || task.generation != runner.current.id {
			runner.rejectChildRequest(frame, codeInvalidParams, "unknown taskId")
			return
		}
	}
	if err := runner.writeHost(frame.raw); err != nil {
		runner.terminate(ReasonHostOutputFailure, err)
		return
	}
	runner.child.register(frame, runner.current.id)
}

func (runner *runner) handleChildCancellation(frame *parsedFrame) {
	if frame.utilityErr != nil || frame.cancellation == nil || runner.current == nil {
		return
	}
	record := runner.child.requests[frame.cancellation.requestID.key]
	if record == nil || record.generation != runner.current.id || record.initialize || record.taskAugmented {
		return
	}
	if err := runner.writeHost(frame.raw); err != nil {
		runner.terminate(ReasonHostOutputFailure, err)
		return
	}
	runner.child.cancel(record)
}

func (runner *runner) handleChildProgress(frame *parsedFrame) {
	if frame.utilityErr != nil || frame.progress == nil || runner.current == nil {
		return
	}
	if !runner.host.acceptProgress(frame.progress, runner.current.id) {
		return
	}
	if err := runner.writeHost(frame.raw); err != nil {
		runner.terminate(ReasonHostOutputFailure, err)
	}
}

func (runner *runner) handleChildTaskStatus(frame *parsedFrame) {
	if frame.utilityErr != nil || frame.taskStatus == nil || runner.current == nil {
		return
	}
	if !runner.host.updateTask(frame.taskStatus, runner.current.id) {
		return
	}
	if err := runner.writeHost(frame.raw); err != nil {
		runner.terminate(ReasonHostOutputFailure, err)
	}
}

func (runner *runner) commitQuiescingCancellations() {
	if runner.current == nil {
		return
	}
	for _, item := range runner.pending.items {
		if !item.sameGenerationOnly || item.frame.cancellation == nil {
			continue
		}
		record := runner.host.requests[item.frame.cancellation.requestID.key]
		if record == nil || record.generation != runner.current.id || record.initialize || record.taskAugmented {
			continue
		}
		runner.host.cancel(record)
	}
}

func (runner *runner) handleChildControl(frame *parsedFrame) {
	generation := runner.current
	if generation == nil || generation.admission == nil || !generation.admission.Verified() {
		return
	}
	control, err := controlOfParsedFrame(frame)
	if err != nil {
		runner.replaceCurrent(ReasonProtocolFailure)
		return
	}
	switch control {
	case ControlDormantReady:
		if runner.state != StateActive || runner.initializeAwaiting || runner.initialized == nil {
			runner.replaceCurrent(ReasonProtocolFailure)
			return
		}
		observedSequence, _ := runner.hostBoundary.snapshot()
		if !runner.safeToCommitDormant(observedSequence) {
			runner.emit(StateActive, ReasonDormantRejected)
			return
		}
		runner.dormantTransitionStart = observedSequence
		runner.setState(StateQuiescing, ReasonDormantCommit)
		outcome, writeErr := runner.writeChild(ProtocolV2().Frame(ControlCommitDormant))
		if outcome != writeDelivered || writeErr != nil {
			runner.replaceCurrent(ReasonWriteFailure)
			return
		}
		runner.dormantExitTimer = runner.cfg.runtimeClock.NewTimer(runner.cfg.DormantExitTimeout)
	case ControlDormantNack:
		if runner.state != StateQuiescing {
			runner.replaceCurrent(ReasonProtocolFailure)
			return
		}
		runner.stopTimer(&runner.dormantExitTimer)
		runner.setState(StateActive, ReasonDormantRejected)
		runner.flushPending()
	case ControlDormantAck:
		if runner.state != StateQuiescing {
			runner.replaceCurrent(ReasonProtocolFailure)
			return
		}
		runner.stopTimer(&runner.childExitDrainTimer)
		runner.commitQuiescingCancellations()
		runner.pending.discardSameGenerationOnly()
		runner.setState(StateAwaitingDormantExit, ReasonDormantCommit)
		runner.dormantExitTimer = runner.cfg.runtimeClock.NewTimer(runner.cfg.DormantExitTimeout)
		runner.maybeCompleteDormancy()
	default:
		runner.replaceCurrent(ReasonProtocolFailure)
	}
}

func (runner *runner) safeToCommitDormant(observedSequence uint64) bool {
	if runner.current == nil || runner.pending.len() != 0 || observedSequence != runner.lastHostSequence {
		return false
	}
	return registryIdle(&runner.host) && registryIdle(&runner.child)
}

func registryIdle(registry *directionRegistry) bool {
	return len(registry.requests) == 0 && len(registry.tokens) == 0 && len(registry.tasks) == 0
}

func (runner *runner) rejectHostRequest(frame *parsedFrame, code int, message string) {
	if frame.id == nil {
		return
	}
	if err := runner.writeHost(localErrorFrame(frame.id.raw, code, message)); err != nil {
		runner.terminate(ReasonHostOutputFailure, err)
	}
}

func (runner *runner) rejectChildRequest(frame *parsedFrame, code int, message string) {
	if frame.id == nil || runner.current == nil {
		return
	}
	outcome, err := runner.writeChild(localErrorFrame(frame.id.raw, code, message))
	if outcome != writeDelivered || err != nil {
		runner.replaceCurrent(ReasonWriteFailure)
	}
}

func (runner *runner) writeHost(frame []byte) error {
	outcome, err := writeFrame(runner.cfg.HostOut, frame)
	if outcome != writeDelivered {
		if err == nil {
			err = errors.New("host frame was not fully written")
		}
		return fmt.Errorf("supervisor: host output: %w", err)
	}
	if err != nil {
		return fmt.Errorf("supervisor: host output: %w", err)
	}
	return nil
}

func (runner *runner) writeChild(frame []byte) (writeOutcome, error) {
	if runner.current == nil {
		return writeNotWritten, errors.New("supervisor: no active child")
	}
	return writeFrame(runner.current.stdin, frame)
}

func isResponse(frame *parsedFrame) bool {
	return frame.kind == frameResult || frame.kind == frameError
}
