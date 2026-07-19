package supervisor

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

type typedNilChild struct{}

func (*typedNilChild) Stdin() io.WriteCloser { panic("typed-nil child called") }
func (*typedNilChild) Stdout() io.ReadCloser { panic("typed-nil child called") }
func (*typedNilChild) Wait() <-chan Exit     { panic("typed-nil child called") }
func (*typedNilChild) Stop(context.Context) error {
	panic("typed-nil child called")
}

type typedNilAdmission struct{}

func (*typedNilAdmission) Verified() bool { panic("typed-nil admission called") }
func (*typedNilAdmission) Close() error   { panic("typed-nil admission called") }

func TestConfigDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	cfg, err := (Config{
		HostIn:  io.NopCloser(bytes.NewReader(nil)),
		HostOut: new(bytes.Buffer),
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start:   func(context.Context, EngineRef) (StartResult, error) { return StartResult{}, nil },
	}).withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RetryDelay != defaultRetryDelay ||
		cfg.StartTimeout != defaultStartTimeout ||
		cfg.ReplayTimeout != defaultReplayTimeout ||
		cfg.GracefulStopTimeout != defaultGracefulStopTimeout ||
		cfg.OutputDrainTimeout != defaultOutputDrainTimeout ||
		cfg.DormantExitTimeout != defaultDormantExitTimeout ||
		cfg.MaxFrameBytes != defaultMaxFrameBytes ||
		cfg.PendingFrameLimit != defaultPendingFrameLimit ||
		cfg.PendingByteLimit != defaultPendingByteLimit {
		t.Fatalf("defaults = %#v", cfg)
	}

	if _, err := (Config{}).withDefaults(); err == nil {
		t.Fatal("empty config unexpectedly passed validation")
	}
}

func TestLifecycleLoggerExcludesOpaqueEngineIdentities(t *testing.T) {
	var output bytes.Buffer
	runner := &runner{
		cfg:        Config{Logger: log.New(&output, "", 0)},
		generation: 7,
		current: &generation{
			requested: EngineRef{ID: `C:\\secret\\requested.exe`},
			actual:    EngineRef{ID: `C:\\secret\\actual.exe`},
			fallback:  true,
		},
	}
	runner.emit(StateActive, ReasonEngineSwitch)
	logged := output.String()
	for _, forbidden := range []string{"secret", "requested.exe", "actual.exe"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("lifecycle log leaked %q: %s", forbidden, logged)
		}
	}
	for _, required := range []string{"generation=7", "state=active", "reason=engine_switch", "fallback=true"} {
		if !strings.Contains(logged, required) {
			t.Fatalf("lifecycle log missing %q: %s", required, logged)
		}
	}
}

func TestTypedNilStartAuthoritiesAreAbsent(t *testing.T) {
	var child *typedNilChild
	var admission *typedNilAdmission
	result := StartResult{Child: child, Admission: admission}
	if startResultHasAuthority(result) {
		t.Fatal("typed-nil start result reported live authority")
	}
	runner := &runner{cfg: Config{GracefulStopTimeout: time.Second}}
	if err := runner.cleanupInvalidStart(result); err != nil {
		t.Fatalf("typed-nil cleanup = %v", err)
	}
	if _, err := newGeneration(context.Background(), 1, EngineRef{ID: "engine"}, result, 1024, make(chan any)); err == nil {
		t.Fatal("typed-nil child was accepted as a generation")
	}
}
