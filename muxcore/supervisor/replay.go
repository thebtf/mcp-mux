package supervisor

func (runner *runner) beginReplay() {
	if runner.current == nil || runner.initializeRequest == nil || runner.initializeID == nil {
		runner.replaceCurrent(ReasonProtocolFailure)
		return
	}
	runner.replayPrelude = nil
	runner.replayPreludeBytes = 0
	runner.replayResponse = nil
	runner.setState(StateReplaying, ReasonInitial)
	outcome, err := runner.writeChild(runner.initializeRequest)
	if outcome != writeDelivered || err != nil {
		runner.replaceCurrent(ReasonWriteFailure)
		return
	}
	runner.replayTimer = runner.cfg.runtimeClock.NewTimer(runner.cfg.ReplayTimeout)
	runner.flushReplayPings()
}

func (runner *runner) handleReplayFrame(frame *parsedFrame) {
	if runner.current == nil || runner.state != StateReplaying {
		return
	}
	if isInitializationPing(frame) {
		runner.handleChildRequest(frame)
		return
	}
	if isResponse(frame) {
		if frame.id != nil {
			if record := runner.host.requests[frame.id.key]; record != nil && record.generation == runner.current.id {
				runner.handleChildResponse(frame)
				return
			}
		}
		if frame.id == nil || runner.initializeID == nil || frame.id.key != runner.initializeID.key {
			runner.replaceCurrent(ReasonProtocolFailure)
			return
		}
		if runner.initializeResponse != nil && frame.kind != frameResult {
			runner.replaceCurrent(ReasonProtocolFailure)
			return
		}
		runner.replayResponse = append([]byte(nil), frame.raw...)
		runner.completeReplay(frame)
		return
	}
	if runner.initializeAwaiting {
		if !isInitializationLogging(frame) || frame.utilityErr != nil {
			runner.replaceCurrent(ReasonProtocolFailure)
			return
		}
		if err := runner.writeHost(frame.raw); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
		}
		return
	}
	if !isInitializationLogging(frame) || frame.utilityErr != nil {
		runner.replaceCurrent(ReasonProtocolFailure)
		return
	}
	size := len(frame.raw) + 1
	if len(runner.replayPrelude) >= runner.cfg.PendingFrameLimit || size > runner.cfg.PendingByteLimit-runner.replayPreludeBytes {
		runner.replaceCurrent(ReasonProtocolFailure)
		return
	}
	runner.replayPrelude = append(runner.replayPrelude, frame.raw)
	runner.replayPreludeBytes += size
}

func (runner *runner) flushReplayPings() {
	for !runner.terminal && runner.current != nil && runner.state == StateReplaying {
		item := runner.pending.popFirstInitializationPing()
		if item == nil {
			return
		}
		runner.deliverHostFirst(item)
	}
}

func (runner *runner) completeReplay(response *parsedFrame) {
	runner.stopTimer(&runner.replayTimer)
	if runner.current == nil {
		return
	}
	if runner.initialized != nil {
		outcome, err := runner.writeChild(runner.initialized)
		if outcome != writeDelivered || err != nil {
			runner.replaceCurrent(ReasonWriteFailure)
			return
		}
	}

	for _, prelude := range runner.replayPrelude {
		if err := runner.writeHost(prelude); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
			return
		}
	}
	runner.replayPrelude = nil
	runner.replayPreludeBytes = 0

	if runner.initializeAwaiting {
		if err := runner.writeHost(response.raw); err != nil {
			runner.terminate(ReasonHostOutputFailure, err)
			return
		}
		runner.initializeAwaiting = false
		if response.kind == frameResult {
			runner.initializeResponse = append([]byte(nil), response.raw...)
		} else {
			runner.initializeFailed = true
		}
	}

	if runner.initialized != nil && response.kind == frameResult {
		for _, kind := range runner.cfg.ReplacementNotifications {
			if !supportsListChanged(runner.initializeResponse, kind) {
				continue
			}
			if err := runner.writeHost(listChangedFrame(kind)); err != nil {
				runner.terminate(ReasonHostOutputFailure, err)
				return
			}
		}
	}

	runner.replayResponse = nil
	runner.host.clearTransitionTombstones()
	runner.child.clearTransitionTombstones()
	runner.setState(StateActive, ReasonInitial)
	runner.flushPending()
}

func (runner *runner) completeInitialInitialize(response *parsedFrame) {
	if !runner.initializeAwaiting {
		return
	}
	runner.stopTimer(&runner.replayTimer)
	if err := runner.writeHost(response.raw); err != nil {
		runner.terminate(ReasonHostOutputFailure, err)
		return
	}
	runner.initializeAwaiting = false
	if response.kind == frameResult {
		runner.initializeResponse = append([]byte(nil), response.raw...)
	} else {
		runner.initializeFailed = true
	}
}

func (runner *runner) flushPending() {
	for !runner.terminal && runner.current != nil && runner.state == StateActive {
		item := runner.pending.pop()
		if item == nil {
			return
		}
		runner.handleHostEvent(hostFrameEvent{
			sequence: item.sequence,
			arrived:  item.arrived,
			frame:    item.frame,
		})
	}
}
