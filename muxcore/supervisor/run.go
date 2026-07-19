package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

type hostFrameEvent struct {
	sequence uint64
	arrived  time.Time
	frame    *parsedFrame
	err      error
}

type hostTerminalEvent struct{ err error }

type startAttemptEvent struct {
	attempt   uint64
	requested EngineRef
	result    StartResult
	err       error
}

type resolveEvent struct {
	attempt uint64
	ref     EngineRef
	err     error
}

type hostBoundary struct {
	mu       sync.Mutex
	sequence uint64
	arrived  time.Time
}

func (boundary *hostBoundary) record(arrived time.Time) uint64 {
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	boundary.sequence++
	boundary.arrived = arrived
	return boundary.sequence
}

func (boundary *hostBoundary) snapshot() (uint64, time.Time) {
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	return boundary.sequence, boundary.arrived
}

type runner struct {
	ctx    context.Context
	cancel context.CancelFunc
	cfg    Config

	events           chan any
	hostDone         chan struct{}
	hostBoundary     hostBoundary
	lastHostSequence uint64
	background       sync.WaitGroup

	state       State
	generation  uint64
	current     *generation
	starting    bool
	startID     uint64
	startCancel context.CancelFunc
	resolving   bool
	resolveID   uint64

	startTimer          timer
	retryTimer          timer
	replayTimer         timer
	dormantExitTimer    timer
	childExitDrainTimer timer
	dormantLeaseTimer   timer
	reconcileTicker     ticker

	pending pendingQueue
	host    directionRegistry
	child   directionRegistry

	initializeRequest      []byte
	initializeID           *scalarValue
	initializeAwaiting     bool
	initializeResponse     []byte
	initializeFailed       bool
	initialized            []byte
	replayPrelude          [][]byte
	replayPreludeBytes     int
	replayResponse         []byte
	dormancyInvalid        bool
	dormantTransitionStart uint64
	dormantLeaseDeadline   time.Time

	terminal       bool
	terminalReason Reason
	terminalErr    error
}

// Run owns one stable host transport until host EOF, caller cancellation,
// dormant lease expiry, or an unrecoverable transport/finalization failure.
func Run(parent context.Context, config Config) error {
	cfg, err := config.withDefaults()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	runner := &runner{
		ctx:      ctx,
		cancel:   cancel,
		cfg:      cfg,
		events:   make(chan any),
		hostDone: make(chan struct{}),
		host:     newDirectionRegistry(),
		child:    newDirectionRegistry(),
	}
	defer runner.cancel()

	go runner.readHost()
	if cfg.ReconcileInterval > 0 {
		runner.reconcileTicker = cfg.runtimeClock.NewTicker(cfg.ReconcileInterval)
	}
	runner.beginStart()

	for !runner.terminal {
		select {
		case <-parent.Done():
			runner.terminate(ReasonCallerCanceled, parent.Err())
		case event := <-runner.events:
			runner.handleEvent(event)
		case <-timerChannel(runner.retryTimer):
			runner.stopTimer(&runner.retryTimer)
			runner.beginStart()
		case <-timerChannel(runner.startTimer):
			runner.stopTimer(&runner.startTimer)
			if runner.startCancel != nil {
				runner.startCancel()
				runner.startCancel = nil
			}
			runner.starting = false
			runner.startID++
			runner.terminate(ReasonProtocolFailure, fmt.Errorf("supervisor: resolve/start attempt timed out"))
		case <-timerChannel(runner.replayTimer):
			runner.stopTimer(&runner.replayTimer)
			runner.replaceCurrent(ReasonProtocolFailure)
		case <-timerChannel(runner.childExitDrainTimer):
			runner.stopTimer(&runner.childExitDrainTimer)
			if runner.current != nil {
				_ = runner.current.stdout.Close()
				runner.replaceCurrent(ReasonCrash)
			}
		case <-timerChannel(runner.dormantExitTimer):
			runner.stopTimer(&runner.dormantExitTimer)
			runner.replaceCurrent(ReasonDormantRejected)
		case now := <-timerChannel(runner.dormantLeaseTimer):
			runner.handleDormantLease(now)
		case <-tickerChannel(runner.reconcileTicker):
			runner.beginResolve()
		}
	}
	return runner.shutdown()
}

func (runner *runner) readHost() {
	defer close(runner.hostDone)
	readerSize := runner.cfg.MaxFrameBytes + 2
	if readerSize > 64*1024 {
		readerSize = 64 * 1024
	}
	reader := bufio.NewReaderSize(runner.cfg.HostIn, readerSize)
	for {
		raw, err := readBoundedLine(reader, runner.cfg.MaxFrameBytes)
		if err != nil {
			select {
			case runner.events <- hostTerminalEvent{err: err}:
			case <-runner.ctx.Done():
			}
			return
		}
		arrived := runner.cfg.runtimeClock.Now()
		sequence := runner.hostBoundary.record(arrived)
		frame, parseErr := parseFrame(raw, runner.cfg.MaxFrameBytes)
		event := hostFrameEvent{sequence: sequence, arrived: arrived, frame: frame, err: parseErr}
		select {
		case runner.events <- event:
		case <-runner.ctx.Done():
			return
		}
	}
}

func (runner *runner) handleEvent(event any) {
	switch event := event.(type) {
	case hostFrameEvent:
		runner.handleHostEvent(event)
	case hostTerminalEvent:
		if errors.Is(event.err, io.EOF) {
			runner.terminate(ReasonHostEOF, nil)
		} else {
			runner.terminate(ReasonHostEOF, fmt.Errorf("supervisor: host input: %w", event.err))
		}
	case startAttemptEvent:
		runner.handleStartAttempt(event)
	case resolveEvent:
		runner.handleResolve(event)
	case childFrameEvent:
		if runner.current != nil && event.generation == runner.current.id {
			runner.handleChildFrame(event.frame)
		}
	case childOutputEvent:
		if runner.current != nil && event.generation == runner.current.id {
			runner.handleChildOutput(event.err)
		}
	case childExitEvent:
		if runner.current != nil && event.generation == runner.current.id {
			runner.handleChildExit(event.exit)
		}
	case childProofEvent:
		if runner.current != nil && event.generation == runner.current.id {
			runner.terminate(ReasonProtocolFailure, fmt.Errorf("supervisor: %w", event.err))
		}
	}
}

func (runner *runner) beginStart() {
	if runner.terminal || runner.starting || runner.current != nil {
		return
	}
	runner.stopTimer(&runner.retryTimer)
	runner.starting = true
	runner.startID++
	attempt := runner.startID
	reason := ReasonRetry
	if runner.generation == 0 && attempt == 1 {
		reason = ReasonInitial
	}
	runner.setState(StateStarting, reason)
	attemptCtx, attemptCancel := context.WithCancel(runner.ctx)
	runner.startCancel = attemptCancel
	runner.startTimer = runner.cfg.runtimeClock.NewTimer(runner.cfg.StartTimeout)

	runner.background.Add(1)
	go func() {
		defer runner.background.Done()
		requested, err := runner.cfg.Resolve(attemptCtx)
		var result StartResult
		if err == nil {
			result, err = runner.cfg.Start(attemptCtx, requested)
		}
		event := startAttemptEvent{attempt: attempt, requested: requested, result: result, err: err}
		select {
		case runner.events <- event:
		case <-runner.ctx.Done():
			if startResultHasAuthority(result) {
				_ = runner.cleanupInvalidStart(result)
			}
		}
	}()
}

func (runner *runner) handleStartAttempt(event startAttemptEvent) {
	if event.attempt != runner.startID || !runner.starting || runner.terminal {
		if startResultHasAuthority(event.result) {
			if cleanupErr := runner.cleanupInvalidStart(event.result); cleanupErr != nil && !runner.terminal {
				runner.terminate(ReasonProtocolFailure, cleanupErr)
			}
		}
		return
	}
	runner.stopTimer(&runner.startTimer)
	if runner.startCancel != nil {
		runner.startCancel()
		runner.startCancel = nil
	}
	runner.starting = false
	if event.err != nil {
		hadAuthority := startResultHasAuthority(event.result)
		if hadAuthority {
			if cleanupErr := runner.cleanupInvalidStart(event.result); cleanupErr != nil {
				runner.terminate(ReasonProtocolFailure, errors.Join(event.err, cleanupErr))
				return
			}
		}
		if errors.Is(event.err, ErrStartRollbackUnproven) && !hadAuthority {
			runner.terminate(ReasonProtocolFailure, event.err)
			return
		}
		runner.scheduleRetry()
		return
	}

	nextID := runner.generation + 1
	generation, err := newGeneration(runner.ctx, nextID, event.requested, event.result, runner.cfg.MaxFrameBytes, runner.events)
	if err != nil {
		if cleanupErr := runner.cleanupInvalidStart(event.result); cleanupErr != nil {
			runner.terminate(ReasonProtocolFailure, errors.Join(err, cleanupErr))
			return
		}
		runner.scheduleRetry()
		return
	}
	runner.generation = nextID
	runner.current = generation
	runner.dormancyInvalid = false

	if runner.initializeRequest != nil {
		runner.beginReplay()
		return
	}
	runner.setState(StateActive, ReasonInitial)
	runner.flushPending()
}

func (runner *runner) cleanupInvalidStart(result StartResult) error {
	var cleanupErr error
	if !isNilInterface(result.Child) {
		ctx, cancel := context.WithTimeout(context.Background(), runner.cfg.GracefulStopTimeout)
		stopErr := result.Child.Stop(ctx)
		wait := result.Child.Wait()
		if wait == nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("supervisor: invalid child has no terminal proof"))
		} else {
			select {
			case exit, ok := <-wait:
				if !ok {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("supervisor: invalid child terminal proof channel closed empty"))
				} else if errors.Is(exit.Err, ErrProcessTreeUnproven) {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("supervisor: invalid child process-tree retirement unproven: %w", exit.Err))
				}
			case <-ctx.Done():
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("supervisor: invalid child terminal proof: %w", ctx.Err()))
			}
		}
		cancel()
		if stopErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("supervisor: invalid child cleanup: %w", stopErr))
		}
	}
	if !isNilInterface(result.Admission) {
		if err := result.Admission.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("supervisor: invalid admission cleanup: %w", err))
		}
	}
	return cleanupErr
}

func (runner *runner) scheduleRetry() {
	if runner.terminal || runner.current != nil || runner.retryTimer != nil {
		return
	}
	runner.retryTimer = runner.cfg.runtimeClock.NewTimer(runner.cfg.RetryDelay)
	runner.setState(StateStarting, ReasonRetry)
}

func (runner *runner) beginResolve() {
	if runner.terminal || runner.resolving || runner.current == nil || runner.state != StateActive {
		return
	}
	runner.resolving = true
	runner.resolveID++
	attempt := runner.resolveID
	runner.background.Add(1)
	go func() {
		defer runner.background.Done()
		ref, err := runner.cfg.Resolve(runner.ctx)
		select {
		case runner.events <- resolveEvent{attempt: attempt, ref: ref, err: err}:
		case <-runner.ctx.Done():
		}
	}()
}

func (runner *runner) handleResolve(event resolveEvent) {
	if event.attempt != runner.resolveID || !runner.resolving || runner.terminal {
		return
	}
	runner.resolving = false
	if event.err != nil || runner.current == nil || event.ref.ID == "" {
		return
	}
	if event.ref != runner.current.requested {
		runner.replaceCurrent(ReasonEngineSwitch)
	}
}

func (runner *runner) setState(state State, reason Reason) {
	runner.state = state
	runner.emit(state, reason)
}

func (runner *runner) emit(state State, reason Reason) {
	fallback := runner.current != nil && runner.current.fallback
	if runner.cfg.Logger != nil {
		runner.cfg.Logger.Printf(
			"supervisor: generation=%d state=%s reason=%s fallback=%t",
			runner.generation,
			state,
			reason,
			fallback,
		)
	}
	if runner.cfg.Observe == nil {
		return
	}
	event := Event{Generation: runner.generation, State: state, Reason: reason, Fallback: fallback}
	if runner.current != nil {
		event.Requested = runner.current.requested
		event.Actual = runner.current.actual
	}
	func() {
		defer func() { _ = recover() }()
		runner.cfg.Observe(event)
	}()
}

func (runner *runner) terminate(reason Reason, err error) {
	if runner.terminal {
		return
	}
	runner.terminal = true
	runner.terminalReason = reason
	runner.terminalErr = err
	runner.cancel()
}

func (runner *runner) shutdown() error {
	runner.stopAllTimers()
	runner.setState(StateStopping, runner.terminalReason)

	_ = runner.cfg.HostIn.Close()
	hostTimer := runner.cfg.runtimeClock.NewTimer(runner.cfg.GracefulStopTimeout)
	select {
	case <-runner.hostDone:
		hostTimer.Stop()
	case <-hostTimer.C():
	}
	if runner.current != nil {
		runner.host.retireGeneration(runner.current.id)
		runner.child.retireGeneration(runner.current.id)
		if err := runner.finalizeCurrent(); err != nil && runner.terminalErr == nil {
			runner.terminalErr = err
		}
	}
	runner.pending.clear()
	backgroundDone := make(chan struct{})
	go func() {
		runner.background.Wait()
		close(backgroundDone)
	}()
	backgroundTimer := time.NewTimer(runner.cfg.GracefulStopTimeout)
	select {
	case <-backgroundDone:
		backgroundTimer.Stop()
	case <-backgroundTimer.C:
		if runner.terminalErr == nil {
			runner.terminalErr = fmt.Errorf("supervisor: resolve/start cancellation timed out")
		}
	}
	runner.setState(StateTerminal, runner.terminalReason)
	return runner.terminalErr
}

func (runner *runner) stopAllTimers() {
	runner.stopTimer(&runner.startTimer)
	if runner.startCancel != nil {
		runner.startCancel()
		runner.startCancel = nil
	}
	runner.stopTimer(&runner.retryTimer)
	runner.stopTimer(&runner.replayTimer)
	runner.stopTimer(&runner.dormantExitTimer)
	runner.stopTimer(&runner.childExitDrainTimer)
	runner.stopTimer(&runner.dormantLeaseTimer)
	if runner.reconcileTicker != nil {
		runner.reconcileTicker.Stop()
		runner.reconcileTicker = nil
	}
}

func (runner *runner) stopTimer(target *timer) {
	if *target != nil {
		(*target).Stop()
		*target = nil
	}
}

func timerChannel(timer timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C()
}

func tickerChannel(ticker ticker) <-chan time.Time {
	if ticker == nil {
		return nil
	}
	return ticker.C()
}
