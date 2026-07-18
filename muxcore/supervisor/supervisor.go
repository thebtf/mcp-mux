// Package supervisor keeps one MCP host stdio transport attached to a
// replaceable sequence of child engine processes.
package supervisor

import (
	"context"
	"fmt"
	"io"
	"log"
	"reflect"
	"time"
)

const (
	defaultRetryDelay          = 100 * time.Millisecond
	defaultStartTimeout        = 30 * time.Second
	defaultReplayTimeout       = 30 * time.Second
	defaultGracefulStopTimeout = 2 * time.Second
	defaultOutputDrainTimeout  = 2 * time.Second
	defaultDormantExitTimeout  = 2 * time.Second
	defaultMaxFrameBytes       = 1024 * 1024
	defaultPendingFrameLimit   = 256
	defaultPendingByteLimit    = 8 * 1024 * 1024
)

// EngineRef is the stable equality identity of an engine selected by Resolve.
// The supervisor treats ID as opaque and does not log it by default.
type EngineRef struct {
	ID string
}

// Exit is the single terminal result produced by Child.Wait.
type Exit struct {
	Code int
	Err  error
}

// Child is one supervised engine generation.
type Child interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Wait() <-chan Exit

	// Stop retires the child and its complete process-tree authority. A nil
	// return means the terminal Wait result is already available.
	Stop(context.Context) error
}

// ControlAdmission authorizes private lifecycle control for one child
// generation. A nil or unverified admission leaves private control disabled.
type ControlAdmission interface {
	Verified() bool
	Close() error
}

// StartResult identifies the child that actually started. Actual may differ
// from the requested engine when the product approved a fallback.
type StartResult struct {
	Child     Child
	Actual    EngineRef
	Fallback  bool
	Admission ControlAdmission
}

// ResolveFunc returns the engine the product currently wants to run.
type ResolveFunc func(context.Context) (EngineRef, error)

// StartFunc starts one requested engine generation. Implementations must honor
// context cancellation and roll back every partial process or admission
// authority before returning an error.
type StartFunc func(context.Context, EngineRef) (StartResult, error)

// State is the externally observable supervisor state.
type State uint8

const (
	StateStarting State = iota
	StateReplaying
	StateActive
	StateQuiescing
	StateAwaitingDormantExit
	StateFinalizing
	StateDormant
	StateStopping
	StateTerminal
)

// Reason is the finite, redaction-safe cause vocabulary for lifecycle events.
type Reason uint8

const (
	ReasonInitial Reason = iota
	ReasonCrash
	ReasonOutputEOF
	ReasonWriteFailure
	ReasonProtocolFailure
	ReasonEngineSwitch
	ReasonDormantWake
	ReasonDormantCommit
	ReasonDormantRejected
	ReasonRetry
	ReasonHostEOF
	ReasonHostOutputFailure
	ReasonCallerCanceled
)

// Event is an explicit lifecycle observation for a caller-provided callback.
// Requested and Actual contain the opaque identities supplied by that caller;
// built-in Logger output never includes them, frames, request IDs, arguments,
// environments, tokens, endpoints, or child stderr.
type Event struct {
	Generation uint64
	State      State
	Reason     Reason
	Requested  EngineRef
	Actual     EngineRef
	Fallback   bool
}

// ListChangedKind selects a list-change notification after replacement when
// the initialize response already visible to the host advertised support.
type ListChangedKind uint8

const (
	ListChangedTools ListChangedKind = iota
	ListChangedPrompts
	ListChangedResources
)

func (state State) String() string {
	switch state {
	case StateStarting:
		return "starting"
	case StateReplaying:
		return "replaying"
	case StateActive:
		return "active"
	case StateQuiescing:
		return "quiescing"
	case StateAwaitingDormantExit:
		return "awaiting_dormant_exit"
	case StateFinalizing:
		return "finalizing"
	case StateDormant:
		return "dormant"
	case StateStopping:
		return "stopping"
	case StateTerminal:
		return "terminal"
	default:
		return "unknown"
	}
}

func (reason Reason) String() string {
	switch reason {
	case ReasonInitial:
		return "initial"
	case ReasonCrash:
		return "crash"
	case ReasonOutputEOF:
		return "output_eof"
	case ReasonWriteFailure:
		return "write_failure"
	case ReasonProtocolFailure:
		return "protocol_failure"
	case ReasonEngineSwitch:
		return "engine_switch"
	case ReasonDormantWake:
		return "dormant_wake"
	case ReasonDormantCommit:
		return "dormant_commit"
	case ReasonDormantRejected:
		return "dormant_rejected"
	case ReasonRetry:
		return "retry"
	case ReasonHostEOF:
		return "host_eof"
	case ReasonHostOutputFailure:
		return "host_output_failure"
	case ReasonCallerCanceled:
		return "caller_canceled"
	default:
		return "unknown"
	}
}

// Config defines one stable host transport and its replaceable child policy.
type Config struct {
	HostIn  io.Reader
	HostOut io.Writer
	Logger  *log.Logger

	Resolve ResolveFunc
	Start   StartFunc
	Observe func(Event)

	ReplacementNotifications []ListChangedKind

	RetryDelay          time.Duration
	ReconcileInterval   time.Duration
	StartTimeout        time.Duration
	ReplayTimeout       time.Duration
	GracefulStopTimeout time.Duration
	OutputDrainTimeout  time.Duration
	DormantExitTimeout  time.Duration
	DormantLease        time.Duration

	MaxFrameBytes     int
	PendingFrameLimit int
	PendingByteLimit  int

	runtimeClock clock
}

func (cfg Config) withDefaults() (Config, error) {
	if cfg.HostIn == nil {
		return Config{}, fmt.Errorf("supervisor: host input is nil")
	}
	if cfg.HostOut == nil {
		return Config{}, fmt.Errorf("supervisor: host output is nil")
	}
	if cfg.Resolve == nil {
		return Config{}, fmt.Errorf("supervisor: resolve function is nil")
	}
	if cfg.Start == nil {
		return Config{}, fmt.Errorf("supervisor: start function is nil")
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = defaultRetryDelay
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = defaultStartTimeout
	}
	if cfg.ReplayTimeout == 0 {
		cfg.ReplayTimeout = defaultReplayTimeout
	}
	if cfg.GracefulStopTimeout == 0 {
		cfg.GracefulStopTimeout = defaultGracefulStopTimeout
	}
	if cfg.OutputDrainTimeout == 0 {
		cfg.OutputDrainTimeout = defaultOutputDrainTimeout
	}
	if cfg.DormantExitTimeout == 0 {
		cfg.DormantExitTimeout = defaultDormantExitTimeout
	}
	if cfg.MaxFrameBytes == 0 {
		cfg.MaxFrameBytes = defaultMaxFrameBytes
	}
	if cfg.PendingFrameLimit == 0 {
		cfg.PendingFrameLimit = defaultPendingFrameLimit
	}
	if cfg.PendingByteLimit == 0 {
		cfg.PendingByteLimit = defaultPendingByteLimit
	}
	if cfg.RetryDelay < 0 || cfg.StartTimeout < 0 || cfg.ReplayTimeout < 0 || cfg.GracefulStopTimeout < 0 || cfg.OutputDrainTimeout < 0 || cfg.DormantExitTimeout < 0 {
		return Config{}, fmt.Errorf("supervisor: required duration is negative")
	}
	if cfg.MaxFrameBytes < 1 || cfg.PendingFrameLimit < 1 || cfg.PendingByteLimit < 1 {
		return Config{}, fmt.Errorf("supervisor: frame and pending limits must be positive")
	}
	if cfg.runtimeClock == nil {
		cfg.runtimeClock = realClock{}
	}
	return cfg, nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func startResultHasAuthority(result StartResult) bool {
	return !isNilInterface(result.Child) || !isNilInterface(result.Admission)
}
