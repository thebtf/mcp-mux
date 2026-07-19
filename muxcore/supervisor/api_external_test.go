package supervisor_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/supervisor"
)

type externalChild struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	wait   <-chan supervisor.Exit
}

func (child externalChild) Stdin() io.WriteCloser        { return child.stdin }
func (child externalChild) Stdout() io.ReadCloser        { return child.stdout }
func (child externalChild) Wait() <-chan supervisor.Exit { return child.wait }
func (externalChild) Stop(context.Context) error         { return nil }

type externalAdmission struct{}

func (externalAdmission) Verified() bool { return true }
func (externalAdmission) Close() error   { return nil }

func TestPublicContractCompilesExternally(t *testing.T) {
	input := io.NopCloser(bytes.NewBufferString(""))
	output := new(bytes.Buffer)
	exits := make(chan supervisor.Exit, 1)

	cfg := supervisor.Config{
		HostIn:  input,
		HostOut: output,
		Resolve: func(context.Context) (supervisor.EngineRef, error) {
			return supervisor.EngineRef{ID: "engine-a"}, nil
		},
		Start: func(context.Context, supervisor.EngineRef) (supervisor.StartResult, error) {
			return supervisor.StartResult{
				Child: externalChild{
					stdin:  nopWriteCloser{Writer: io.Discard},
					stdout: io.NopCloser(bytes.NewReader(nil)),
					wait:   exits,
				},
				Actual:    supervisor.EngineRef{ID: "engine-a"},
				Admission: externalAdmission{},
			}, nil
		},
		Observe: func(event supervisor.Event) {
			_ = event.Generation
			_ = event.State
			_ = event.Reason
			_ = event.Requested
			_ = event.Actual
			_ = event.Fallback
		},
		ReplacementNotifications: []supervisor.ListChangedKind{
			supervisor.ListChangedTools,
			supervisor.ListChangedPrompts,
			supervisor.ListChangedResources,
		},
	}
	_ = cfg
	var run func(context.Context, supervisor.Config) error = supervisor.Run
	_ = run
	var startCommand func(context.Context, supervisor.Command) (*supervisor.CommandChild, error) = supervisor.StartCommand
	_ = startCommand
	_ = supervisor.Command{
		Path:   "engine",
		Args:   []string{"serve"},
		Env:    []string{"A=B"},
		Dir:    ".",
		Stderr: io.Discard,
	}
	_ = supervisor.ErrStartRollbackUnproven
	_ = supervisor.ProtocolV2().Frame(supervisor.ControlDormantReady)
	_ = []supervisor.State{
		supervisor.StateStarting,
		supervisor.StateReplaying,
		supervisor.StateActive,
		supervisor.StateQuiescing,
		supervisor.StateAwaitingDormantExit,
		supervisor.StateFinalizing,
		supervisor.StateDormant,
		supervisor.StateStopping,
		supervisor.StateTerminal,
	}
	_ = []supervisor.Reason{
		supervisor.ReasonInitial,
		supervisor.ReasonCrash,
		supervisor.ReasonOutputEOF,
		supervisor.ReasonWriteFailure,
		supervisor.ReasonProtocolFailure,
		supervisor.ReasonEngineSwitch,
		supervisor.ReasonDormantWake,
		supervisor.ReasonDormantCommit,
		supervisor.ReasonDormantRejected,
		supervisor.ReasonRetry,
		supervisor.ReasonHostEOF,
		supervisor.ReasonHostOutputFailure,
		supervisor.ReasonCallerCanceled,
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
