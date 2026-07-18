package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
)

type childFrameEvent struct {
	generation uint64
	frame      *parsedFrame
}

type childOutputEvent struct {
	generation uint64
	err        error
}

type childExitEvent struct {
	generation uint64
	exit       Exit
}

type childProofEvent struct {
	generation uint64
	err        error
}

type generation struct {
	id        uint64
	requested EngineRef
	actual    EngineRef
	fallback  bool

	child     Child
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	admission ControlAdmission

	cancel   context.CancelFunc
	pumpDone chan struct{}
	reapDone chan struct{}
	exitDone chan struct{}

	mu        sync.Mutex
	exit      Exit
	outputEOF bool
	outputErr error
}

func newGeneration(
	ctx context.Context,
	id uint64,
	requested EngineRef,
	result StartResult,
	maxFrameBytes int,
	events chan<- any,
) (*generation, error) {
	if requested.ID == "" {
		return nil, fmt.Errorf("supervisor: resolver returned an empty engine identity")
	}
	if isNilInterface(result.Child) {
		return nil, fmt.Errorf("supervisor: start returned a nil child")
	}
	if result.Actual.ID == "" {
		return nil, fmt.Errorf("supervisor: start returned an empty actual identity")
	}
	if result.Actual != requested && !result.Fallback {
		return nil, fmt.Errorf("supervisor: start changed identity without reporting fallback")
	}

	stdin := result.Child.Stdin()
	stdout := result.Child.Stdout()
	wait := result.Child.Wait()
	if stdin == nil || stdout == nil || wait == nil {
		return nil, fmt.Errorf("supervisor: child returned a nil transport or wait channel")
	}

	admission := result.Admission
	if isNilInterface(admission) {
		admission = nil
	}

	generationCtx, cancel := context.WithCancel(ctx)
	generation := &generation{
		id:        id,
		requested: requested,
		actual:    result.Actual,
		fallback:  result.Fallback,
		child:     result.Child,
		stdin:     stdin,
		stdout:    stdout,
		admission: admission,
		cancel:    cancel,
		pumpDone:  make(chan struct{}),
		reapDone:  make(chan struct{}),
		exitDone:  make(chan struct{}),
	}

	go generation.pump(generationCtx, maxFrameBytes, events)
	go generation.reap(generationCtx, wait, events)
	return generation, nil
}

func (generation *generation) pump(ctx context.Context, maxFrameBytes int, events chan<- any) {
	defer close(generation.pumpDone)
	readerSize := maxFrameBytes + 2
	if readerSize > 64*1024 {
		readerSize = 64 * 1024
	}
	reader := bufio.NewReaderSize(generation.stdout, readerSize)
	for {
		raw, err := readBoundedLine(reader, maxFrameBytes)
		if err != nil {
			generation.mu.Lock()
			generation.outputEOF = err == io.EOF
			generation.outputErr = err
			generation.mu.Unlock()
			select {
			case events <- childOutputEvent{generation: generation.id, err: err}:
			case <-ctx.Done():
			}
			return
		}
		frame, err := parseFrame(raw, maxFrameBytes)
		if err != nil {
			generation.mu.Lock()
			generation.outputErr = err
			generation.mu.Unlock()
			select {
			case events <- childOutputEvent{generation: generation.id, err: err}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case events <- childFrameEvent{generation: generation.id, frame: frame}:
		case <-ctx.Done():
			return
		}
	}
}

func (generation *generation) reap(ctx context.Context, wait <-chan Exit, events chan<- any) {
	defer close(generation.reapDone)
	exit, ok := <-wait
	if !ok {
		select {
		case events <- childProofEvent{
			generation: generation.id,
			err:        fmt.Errorf("child wait channel closed without a terminal result"),
		}:
		case <-ctx.Done():
		}
		return
	}
	generation.mu.Lock()
	generation.exit = exit
	generation.mu.Unlock()
	close(generation.exitDone)
	select {
	case events <- childExitEvent{generation: generation.id, exit: exit}:
	case <-ctx.Done():
	}
}

func (generation *generation) exitResult() (Exit, bool) {
	select {
	case <-generation.exitDone:
		generation.mu.Lock()
		exit := generation.exit
		generation.mu.Unlock()
		return exit, true
	default:
		return Exit{}, false
	}
}

func (generation *generation) outputTerminal() (bool, error) {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	return generation.outputEOF, generation.outputErr
}
