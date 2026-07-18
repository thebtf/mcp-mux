package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type testAdmission struct {
	verified bool
	closed   chan struct{}
	once     sync.Once
}

func newTestAdmission(verified bool) *testAdmission {
	return &testAdmission{verified: verified, closed: make(chan struct{})}
}

func (admission *testAdmission) Verified() bool { return admission.verified }
func (admission *testAdmission) Close() error {
	admission.once.Do(func() { close(admission.closed) })
	return nil
}

type testChild struct {
	stdinWriter  *io.PipeWriter
	inputReader  *io.PipeReader
	outputWriter *io.PipeWriter
	stdoutReader *io.PipeReader
	wait         chan Exit
	stopped      chan struct{}
	once         sync.Once
}

func newTestChild() *testChild {
	inputReader, stdinWriter := io.Pipe()
	stdoutReader, outputWriter := io.Pipe()
	return &testChild{
		stdinWriter:  stdinWriter,
		inputReader:  inputReader,
		outputWriter: outputWriter,
		stdoutReader: stdoutReader,
		wait:         make(chan Exit, 1),
		stopped:      make(chan struct{}),
	}
}

func (child *testChild) Stdin() io.WriteCloser      { return child.stdinWriter }
func (child *testChild) Stdout() io.ReadCloser      { return child.stdoutReader }
func (child *testChild) Wait() <-chan Exit          { return child.wait }
func (child *testChild) Stop(context.Context) error { child.finish(0, nil); return nil }
func (child *testChild) write(raw string) error {
	_, err := io.WriteString(child.outputWriter, raw+"\n")
	return err
}

type faultWriteCloser struct {
	target io.WriteCloser
	mu     sync.Mutex
	armed  bool
	n      int
	err    error
}

func (writer *faultWriteCloser) arm(n int, err error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.armed = true
	writer.n = n
	writer.err = err
}

func (writer *faultWriteCloser) Write(buffer []byte) (int, error) {
	writer.mu.Lock()
	if writer.armed {
		writer.armed = false
		n, err := writer.n, writer.err
		writer.mu.Unlock()
		return n, err
	}
	writer.mu.Unlock()
	return writer.target.Write(buffer)
}

func (writer *faultWriteCloser) Close() error { return writer.target.Close() }

type faultChild struct {
	*testChild
	writer *faultWriteCloser
}

func newFaultChild() *faultChild {
	child := newTestChild()
	return &faultChild{testChild: child, writer: &faultWriteCloser{target: child.stdinWriter}}
}

func (child *faultChild) Stdin() io.WriteCloser { return child.writer }

func (child *testChild) crash(err error) { child.finish(1, err) }
func (child *testChild) dormantExit()    { child.finish(ProtocolV2().DormantExitCode(), nil) }
func (child *testChild) finish(code int, err error) {
	child.once.Do(func() {
		_ = child.outputWriter.Close()
		child.wait <- Exit{Code: code, Err: err}
		close(child.wait)
		close(child.stopped)
	})
}

type lineStream struct {
	lines <-chan string
	errs  <-chan error
}

func streamLines(reader io.Reader) lineStream {
	lines := make(chan string, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(lines)
		buffered := bufio.NewReader(reader)
		for {
			line, err := buffered.ReadString('\n')
			if len(line) > 0 {
				lines <- strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			}
			if err != nil {
				errs <- err
				return
			}
		}
	}()
	return lineStream{lines: lines, errs: errs}
}

func nextLine(t *testing.T, stream lineStream) string {
	t.Helper()
	select {
	case line, ok := <-stream.lines:
		if !ok {
			t.Fatal("line stream closed")
		}
		return line
	case err := <-stream.errs:
		t.Fatalf("line stream failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for line")
	}
	return ""
}

func assertNoLine(t *testing.T, stream lineStream, duration time.Duration) {
	t.Helper()
	select {
	case line := <-stream.lines:
		t.Fatalf("unexpected line: %s", line)
	case err := <-stream.errs:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("line stream failed: %v", err)
		}
	case <-time.After(duration):
	}
}

type runHarness struct {
	hostInput  *io.PipeWriter
	hostOutput lineStream
	done       <-chan error
}

func startHarness(t *testing.T, cfg Config) runHarness {
	t.Helper()
	hostReader, hostInput := io.Pipe()
	hostOutputReader, hostWriter := io.Pipe()
	cfg.HostIn = hostReader
	cfg.HostOut = hostWriter
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), cfg)
		_ = hostWriter.Close()
	}()
	return runHarness{
		hostInput:  hostInput,
		hostOutput: streamLines(hostOutputReader),
		done:       done,
	}
}

func (harness runHarness) send(t *testing.T, raw string) {
	t.Helper()
	if _, err := io.WriteString(harness.hostInput, raw+"\n"); err != nil {
		t.Fatalf("host input write: %v", err)
	}
}

func (harness runHarness) closeAndWait(t *testing.T) {
	t.Helper()
	if err := harness.hostInput.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-harness.done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after host EOF")
	}
}

func TestRunPublicFlow(t *testing.T) {
	child := newTestChild()
	childInput := streamLines(child.inputReader)
	started := make(chan struct{}, 1)
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) {
			return EngineRef{ID: "engine-a"}, nil
		},
		Start: func(context.Context, EngineRef) (StartResult, error) {
			started <- struct{}{}
			return StartResult{Child: child, Actual: EngineRef{ID: "engine-a"}}, nil
		},
	})

	harness.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("child did not start")
	}
	if got := nextLine(t, childInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("child initialize = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":1`) || !strings.Contains(got, `"result"`) {
		t.Fatalf("host initialize response = %s", got)
	}

	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if got := nextLine(t, childInput); !strings.Contains(got, "notifications/initialized") {
		t.Fatalf("child initialized = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"r1","method":"tools/list"}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"id":"r1"`) {
		t.Fatalf("child request = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","id":"r1","result":{"tools":[]}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"r1"`) {
		t.Fatalf("host response = %s", got)
	}

	harness.closeAndWait(t)
	select {
	case <-child.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("child was not stopped")
	}
}

func TestInitialInitializeTimeoutRestartsAndReplays(t *testing.T) {
	first := newTestChild()
	second := newTestChild()
	firstInput := streamLines(first.inputReader)
	secondInput := streamLines(second.inputReader)
	starts := 0
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			starts++
			switch starts {
			case 1:
				return StartResult{Child: first, Actual: EngineRef{ID: "engine"}}, nil
			case 2:
				select {
				case <-first.stopped:
				default:
					return StartResult{}, errors.New("initialize retry preceded predecessor finalization")
				}
				return StartResult{Child: second, Actual: EngineRef{ID: "engine"}}, nil
			default:
				return StartResult{}, errors.New("unexpected extra start")
			}
		},
		RetryDelay:    10 * time.Millisecond,
		ReplayTimeout: 30 * time.Millisecond,
	})

	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{}}`)
	if got := nextLine(t, firstInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("first initialize = %s", got)
	}
	if got := nextLine(t, secondInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("retried initialize = %s", got)
	}
	if err := second.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"second","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"init"`) {
		t.Fatalf("host initialize response = %s", got)
	}
	harness.closeAndWait(t)
}

func TestRunInitializationPermitsPingAndLogging(t *testing.T) {
	child := newTestChild()
	childInput := streamLines(child.inputReader)
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			return StartResult{Child: child, Actual: EngineRef{ID: "engine"}}, nil
		},
	})

	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, childInput)
	if err := child.write(`{"jsonrpc":"2.0","id":"server-ping","method":"ping"}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"server-ping"`) {
		t.Fatalf("server ping = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"server-ping","result":{}}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"id":"server-ping"`) || !strings.Contains(got, `"result":{}`) {
		t.Fatalf("server ping response = %s", got)
	}

	if err := child.write(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"starting"}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, "notifications/message") {
		t.Fatalf("initialization log = %s", got)
	}

	harness.send(t, `{"jsonrpc":"2.0","id":"client-ping","method":"ping"}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"id":"client-ping"`) {
		t.Fatalf("client ping = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","id":"client-ping","result":{}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"client-ping"`) || !strings.Contains(got, `"result":{}`) {
		t.Fatalf("client ping response = %s", got)
	}

	if err := child.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"logging":{}},"serverInfo":{"name":"engine","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, childInput)
	harness.closeAndWait(t)
}

func TestRunFirstInitializeWhileStartingKeepsLifecycleTrafficLive(t *testing.T) {
	child := newTestChild()
	childInput := streamLines(child.inputReader)
	startEntered := make(chan struct{}, 1)
	releaseStart := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseStart) }) }
	t.Cleanup(release)

	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(ctx context.Context, _ EngineRef) (StartResult, error) {
			startEntered <- struct{}{}
			select {
			case <-releaseStart:
				return StartResult{Child: child, Actual: EngineRef{ID: "engine"}}, nil
			case <-ctx.Done():
				return StartResult{}, ctx.Err()
			}
		},
	})

	select {
	case <-startEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("start did not block")
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"ping"}`)
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"init"`) || !strings.Contains(got, "duplicate active request id") {
		t.Fatalf("initialize ID collision = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"client-ping","method":"ping"}`)
	harness.send(t, `{"jsonrpc":"2.0","id":"client-ping","method":"ping"}`)
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"client-ping"`) || !strings.Contains(got, "duplicate pending request id") {
		t.Fatalf("pending ping collision = %s", got)
	}

	release()
	if got := nextLine(t, childInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("first initialize = %s", got)
	}
	if got := nextLine(t, childInput); !strings.Contains(got, `"id":"client-ping"`) {
		t.Fatalf("queued client ping = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"starting-before-init"}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, "starting-before-init") {
		t.Fatalf("live initialization log = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","id":"client-ping","result":{}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"client-ping"`) || !strings.Contains(got, `"result":{}`) {
		t.Fatalf("live client ping response = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","id":"server-ping","method":"ping"}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"server-ping"`) {
		t.Fatalf("live server ping = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"server-ping","result":{}}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"id":"server-ping"`) || !strings.Contains(got, `"result":{}`) {
		t.Fatalf("live server ping response = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"logging":{}},"serverInfo":{"name":"engine","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"init"`) {
		t.Fatalf("initialize response = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if got := nextLine(t, childInput); !strings.Contains(got, "notifications/initialized") {
		t.Fatalf("initialized notification = %s", got)
	}
	harness.closeAndWait(t)
}

func TestRunCrashFinalizesThenReplaysOnlyInitialization(t *testing.T) {
	first := newTestChild()
	second := newTestChild()
	third := newTestChild()
	firstInput := streamLines(first.inputReader)
	secondInput := streamLines(second.inputReader)
	thirdInput := streamLines(third.inputReader)
	firstAdmission := newTestAdmission(true)
	secondAdmission := newTestAdmission(true)
	thirdAdmission := newTestAdmission(true)
	starts := make(chan int, 3)
	var startMu sync.Mutex
	startCount := 0

	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) {
			return EngineRef{ID: "engine-a"}, nil
		},
		Start: func(context.Context, EngineRef) (StartResult, error) {
			startMu.Lock()
			defer startMu.Unlock()
			startCount++
			starts <- startCount
			switch startCount {
			case 1:
				return StartResult{Child: first, Actual: EngineRef{ID: "engine-a"}, Admission: firstAdmission}, nil
			case 2:
				select {
				case <-first.stopped:
				default:
					return StartResult{}, errors.New("successor started before predecessor terminal proof")
				}
				select {
				case <-firstAdmission.closed:
				default:
					return StartResult{}, errors.New("successor started before predecessor admission closed")
				}
				return StartResult{Child: second, Actual: EngineRef{ID: "engine-a"}, Admission: secondAdmission}, nil
			case 3:
				select {
				case <-second.stopped:
				default:
					return StartResult{}, errors.New("third generation started before invalid replay generation terminal proof")
				}
				select {
				case <-secondAdmission.closed:
				default:
					return StartResult{}, errors.New("third generation started before invalid replay admission closed")
				}
				return StartResult{Child: third, Actual: EngineRef{ID: "engine-a"}, Admission: thirdAdmission}, nil
			default:
				return StartResult{}, errors.New("unexpected extra start")
			}
		},
		ReplacementNotifications: []ListChangedKind{ListChangedTools},
		RetryDelay:               10 * time.Millisecond,
	})

	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if got := nextLine(t, firstInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("first initialize = %s", got)
	}
	if err := first.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"first","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, firstInput)

	harness.send(t, `{"jsonrpc":"2.0","id":1e0,"method":"tools/call","params":{"name":"slow","arguments":{}}}`)
	if got := nextLine(t, firstInput); !strings.Contains(got, `"id":1e0`) {
		t.Fatalf("first request = %s", got)
	}
	first.crash(errors.New("boom"))

	lost := nextLine(t, harness.hostOutput)
	if !strings.Contains(lost, `"id":1e0`) || !strings.Contains(lost, lostRequestMessage) {
		t.Fatalf("lost request error = %s", lost)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"after","method":"tools/list"}`)
	select {
	case start := <-starts:
		if start != 1 {
			t.Fatalf("first start marker = %d", start)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing first start marker")
	}
	select {
	case start := <-starts:
		if start != 2 {
			t.Fatalf("second start marker = %d", start)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("successor did not start")
	}

	if got := nextLine(t, secondInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("second replay initialize = %s", got)
	}
	if err := second.write(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case start := <-starts:
		if start != 3 {
			t.Fatalf("third start marker = %d", start)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("invalid replay notification did not retire the generation")
	}
	if got := nextLine(t, thirdInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("third replay initialize = %s", got)
	}
	assertNoLine(t, harness.hostOutput, 100*time.Millisecond)

	harness.send(t, `{"jsonrpc":"2.0","id":"replay-client-ping","method":"ping"}`)
	if got := nextLine(t, thirdInput); !strings.Contains(got, `"id":"replay-client-ping"`) {
		t.Fatalf("replay client ping = %s", got)
	}
	if err := third.write(`{"jsonrpc":"2.0","id":"replay-client-ping","result":{}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"replay-client-ping"`) || !strings.Contains(got, `"result":{}`) {
		t.Fatalf("replay client ping response = %s", got)
	}
	if err := third.write(`{"jsonrpc":"2.0","id":"replay-ping","method":"ping"}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"replay-ping"`) {
		t.Fatalf("replay ping = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"replay-ping","result":{}}`)
	if got := nextLine(t, thirdInput); !strings.Contains(got, `"id":"replay-ping"`) || !strings.Contains(got, `"result":{}`) {
		t.Fatalf("replay ping response = %s", got)
	}
	if err := third.write(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"prelude"}}`); err != nil {
		t.Fatal(err)
	}
	if err := third.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"third","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, thirdInput); !strings.Contains(got, "notifications/initialized") {
		t.Fatalf("replay initialized = %s", got)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, "prelude") {
		t.Fatalf("replay prelude = %s", got)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, "notifications/tools/list_changed") {
		t.Fatalf("replacement notification = %s", got)
	}
	if got := nextLine(t, thirdInput); !strings.Contains(got, `"id":"after"`) {
		t.Fatalf("pending request = %s", got)
	}
	assertNoLine(t, harness.hostOutput, 100*time.Millisecond)
	if err := third.write(`{"jsonrpc":"2.0","id":"after","result":{"tools":[]}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"after"`) {
		t.Fatalf("successor response = %s", got)
	}

	harness.closeAndWait(t)
}

func TestRunReplacementNotificationDoesNotExceedHostCapability(t *testing.T) {
	first := newTestChild()
	second := newTestChild()
	firstInput := streamLines(first.inputReader)
	secondInput := streamLines(second.inputReader)
	starts := 0
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			starts++
			if starts == 1 {
				return StartResult{Child: first, Actual: EngineRef{ID: "engine"}}, nil
			}
			return StartResult{Child: second, Actual: EngineRef{ID: "engine"}}, nil
		},
		ReplacementNotifications: []ListChangedKind{ListChangedTools},
		RetryDelay:               10 * time.Millisecond,
	})

	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, firstInput)
	if err := first.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"first","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, firstInput)
	first.crash(errors.New("boom"))

	_ = nextLine(t, secondInput)
	if err := second.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"second","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, secondInput)
	assertNoLine(t, harness.hostOutput, 100*time.Millisecond)
	harness.closeAndWait(t)
}

func TestRunDormantNackAndWake(t *testing.T) {
	first := newTestChild()
	second := newTestChild()
	firstInput := streamLines(first.inputReader)
	secondInput := streamLines(second.inputReader)
	firstAdmission := newTestAdmission(true)
	secondAdmission := newTestAdmission(true)
	states := make(chan State, 32)
	startCount := 0
	var startMu sync.Mutex

	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) {
			return EngineRef{ID: "engine-a"}, nil
		},
		Start: func(context.Context, EngineRef) (StartResult, error) {
			startMu.Lock()
			defer startMu.Unlock()
			startCount++
			if startCount == 1 {
				return StartResult{Child: first, Actual: EngineRef{ID: "engine-a"}, Admission: firstAdmission}, nil
			}
			if startCount == 2 {
				select {
				case <-firstAdmission.closed:
				default:
					return StartResult{}, errors.New("wake started before admission close")
				}
				return StartResult{Child: second, Actual: EngineRef{ID: "engine-a"}, Admission: secondAdmission}, nil
			}
			return StartResult{}, errors.New("unexpected extra start")
		},
		Observe: func(event Event) { states <- event.State },
	})

	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, firstInput)
	if err := first.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"first","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, firstInput)

	if err := first.write(string(ProtocolV2().Frame(ControlDormantReady))); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, firstInput); got != string(ProtocolV2().Frame(ControlCommitDormant)) {
		t.Fatalf("commit frame = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"queued","method":"tools/list"}`)
	assertNoLine(t, firstInput, 100*time.Millisecond)
	if err := first.write(string(ProtocolV2().Frame(ControlDormantNack))); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, firstInput); !strings.Contains(got, `"id":"queued"`) {
		t.Fatalf("NACK flush = %s", got)
	}
	if err := first.write(`{"jsonrpc":"2.0","id":"queued","result":{"tools":[]}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)

	if err := first.write(string(ProtocolV2().Frame(ControlDormantReady))); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, firstInput); got != string(ProtocolV2().Frame(ControlCommitDormant)) {
		t.Fatalf("second commit frame = %s", got)
	}
	if err := first.write(string(ProtocolV2().Frame(ControlDormantAck))); err != nil {
		t.Fatal(err)
	}
	first.dormantExit()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case state := <-states:
			if state == StateDormant {
				goto dormant
			}
		case <-deadline:
			t.Fatal("supervisor did not enter dormant state")
		}
	}

dormant:
	harness.send(t, `{"jsonrpc":"2.0","id":"wake","method":"tools/list"}`)
	if got := nextLine(t, secondInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("wake replay initialize = %s", got)
	}
	if err := second.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"second","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, secondInput); !strings.Contains(got, "notifications/initialized") {
		t.Fatalf("wake initialized = %s", got)
	}
	if got := nextLine(t, secondInput); !strings.Contains(got, `"id":"wake"`) {
		t.Fatalf("wake request = %s", got)
	}
	assertNoLine(t, harness.hostOutput, 100*time.Millisecond)
	if err := second.write(`{"jsonrpc":"2.0","id":"wake","result":{"tools":[]}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"wake"`) {
		t.Fatalf("wake response = %s", got)
	}
	harness.closeAndWait(t)
}

func TestUnverifiedDormantControlIsSuppressed(t *testing.T) {
	child := newTestChild()
	childInput := streamLines(child.inputReader)
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "legacy"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			return StartResult{Child: child, Actual: EngineRef{ID: "legacy"}, Admission: newTestAdmission(false)}, nil
		},
	})
	harness.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, childInput)
	if err := child.write(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"legacy","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, childInput)
	if err := child.write(string(ProtocolV2().Frame(ControlDormantReady))); err != nil {
		t.Fatal(err)
	}
	assertNoLine(t, childInput, 100*time.Millisecond)
	harness.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"id":2`) {
		t.Fatalf("legacy request = %s", got)
	}
	harness.closeAndWait(t)
}

func TestRunCancellationProgressAndTaskLifetime(t *testing.T) {
	child := newTestChild()
	childInput := streamLines(child.inputReader)
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			return StartResult{Child: child, Actual: EngineRef{ID: "engine"}}, nil
		},
	})
	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, childInput)
	if err := child.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"engine","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, childInput)

	harness.send(t, `{"jsonrpc":"2.0","id":"cancel-me","method":"tools/call","params":{"_meta":{"progressToken":1.0},"name":"slow","arguments":{}}}`)
	_ = nextLine(t, childInput)
	if err := child.write(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":1e0,"progress":0.1}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"progress":0.1`) {
		t.Fatalf("progress = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":1,"progress":0.10}}`); err != nil {
		t.Fatal(err)
	}
	assertNoLine(t, harness.hostOutput, 100*time.Millisecond)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel-me"}}`)
	if got := nextLine(t, childInput); !strings.Contains(got, "notifications/cancelled") {
		t.Fatalf("cancellation = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","id":"cancel-me","result":{"content":[]}}`); err != nil {
		t.Fatal(err)
	}
	assertNoLine(t, harness.hostOutput, 100*time.Millisecond)

	harness.send(t, `{"jsonrpc":"2.0","id":"task-request","method":"tools/call","params":{"_meta":{"progressToken":"task-progress"},"task":{},"name":"task","arguments":{}}}`)
	_ = nextLine(t, childInput)
	if err := child.write(`{"jsonrpc":"2.0","id":"task-request","result":{"task":{"taskId":"task-1","status":"working"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	if err := child.write(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"task-progress","progress":1}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","id":"get-task","method":"tasks/get","params":{"taskId":"task-1"}}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"method":"tasks/get"`) {
		t.Fatalf("task get = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","id":"get-task","result":{"task":{"taskId":"task-1","status":"working"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	if err := child.write(`{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"task-1","status":"completed"}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"status":"completed"`) {
		t.Fatalf("task status = %s", got)
	}
	if err := child.write(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"task-progress","progress":2}}`); err != nil {
		t.Fatal(err)
	}
	assertNoLine(t, harness.hostOutput, 100*time.Millisecond)

	harness.closeAndWait(t)
}

func TestRunChildOriginatedCorrelationIsDirectionSafe(t *testing.T) {
	child := newTestChild()
	childInput := streamLines(child.inputReader)
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			return StartResult{Child: child, Actual: EngineRef{ID: "engine"}}, nil
		},
	})
	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, childInput)
	if err := child.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"engine","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, childInput)

	if err := child.write(`{"jsonrpc":"2.0","id":7,"method":"sampling/createMessage","params":{"_meta":{"progressToken":"child-progress"},"task":{},"messages":[]}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":7`) {
		t.Fatalf("child request = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"child-progress","progress":1}}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"progress":1`) {
		t.Fatalf("host progress = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":7.0,"result":{"task":{"taskId":"child-task","status":"working"}}}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"id":7.0`) {
		t.Fatalf("host response = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"child-progress","progress":2}}`)
	_ = nextLine(t, childInput)
	if err := child.write(`{"jsonrpc":"2.0","id":8,"method":"tasks/get","params":{"taskId":"child-task"}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"method":"tasks/get"`) {
		t.Fatalf("child task request = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":8,"result":{"task":{"taskId":"child-task","status":"working"}}}`)
	_ = nextLine(t, childInput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"child-task","status":"completed"}}`)
	if got := nextLine(t, childInput); !strings.Contains(got, `"status":"completed"`) {
		t.Fatalf("host task status = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"child-progress","progress":3}}`)
	assertNoLine(t, childInput, 100*time.Millisecond)

	if err := child.write(`{"jsonrpc":"2.0","id":"cancel-child","method":"roots/list"}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	if err := child.write(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel-child"}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, "notifications/cancelled") {
		t.Fatalf("child cancellation = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","id":"cancel-child","result":{"roots":[]}}`)
	assertNoLine(t, childInput, 100*time.Millisecond)
	harness.closeAndWait(t)
}

func TestRunPendingCancellationAndQueueOverflow(t *testing.T) {
	first := newTestChild()
	second := newTestChild()
	firstInput := streamLines(first.inputReader)
	secondInput := streamLines(second.inputReader)
	releaseSecond := make(chan struct{})
	secondStarting := make(chan struct{})
	startCount := 0
	var startMu sync.Mutex
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(ctx context.Context, _ EngineRef) (StartResult, error) {
			startMu.Lock()
			startCount++
			count := startCount
			startMu.Unlock()
			if count == 1 {
				return StartResult{Child: first, Actual: EngineRef{ID: "engine"}}, nil
			}
			if count == 2 {
				close(secondStarting)
				select {
				case <-releaseSecond:
					return StartResult{Child: second, Actual: EngineRef{ID: "engine"}}, nil
				case <-ctx.Done():
					return StartResult{}, ctx.Err()
				}
			}
			return StartResult{}, errors.New("unexpected extra start")
		},
		PendingFrameLimit: 1,
		PendingByteLimit:  4096,
	})
	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, firstInput)
	if err := first.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"first","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, firstInput)
	first.crash(errors.New("boom"))
	select {
	case <-secondStarting:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement start did not block")
	}

	harness.send(t, `{"jsonrpc":"2.0","id":"cancel-pending","method":"tools/list"}`)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel-pending"}}`)
	harness.send(t, `{"jsonrpc":"2.0","id":"kept","method":"tools/list"}`)
	harness.send(t, `{"jsonrpc":"2.0","id":"overflow","method":"tools/list"}`)
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"overflow"`) || !strings.Contains(got, "pending queue is full") {
		t.Fatalf("overflow error = %s", got)
	}
	close(releaseSecond)
	_ = nextLine(t, secondInput)
	if err := second.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"second","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, secondInput)
	if got := nextLine(t, secondInput); !strings.Contains(got, `"id":"kept"`) {
		t.Fatalf("retained request = %s", got)
	}
	assertNoLine(t, secondInput, 100*time.Millisecond)
	harness.closeAndWait(t)
}

func TestRunStrictLifecyclePrivateNamespaceAndDuplicates(t *testing.T) {
	child := newTestChild()
	childInput := streamLines(child.inputReader)
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			return StartResult{Child: child, Actual: EngineRef{ID: "engine"}}, nil
		},
	})
	harness.send(t, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":9`) || !strings.Contains(got, "initialize must be the first request") {
		t.Fatalf("pre-initialize error = %s", got)
	}
	harness.send(t, `{"jsonrpc":"2.0","method":"$/mcp-mux/launcher/dormant-ready"}`)
	assertNoLine(t, childInput, 100*time.Millisecond)
	harness.send(t, `{"jsonrpc":"2.0","id":"private","method":"$/mcp-mux/launcher/dormant-ready"}`)
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"private"`) || !strings.Contains(got, "method not found") {
		t.Fatalf("private method error = %s", got)
	}

	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, childInput)
	if err := child.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"engine","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, childInput)

	harness.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_ = nextLine(t, childInput)
	harness.send(t, `{"jsonrpc":"2.0","id":1.0,"method":"tools/list"}`)
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":1.0`) || !strings.Contains(got, "duplicate active request id") {
		t.Fatalf("duplicate ID error = %s", got)
	}
	assertNoLine(t, childInput, 100*time.Millisecond)
	if err := child.write(`{"jsonrpc":"2.0","id":1e0,"result":{"tools":[]}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)

	harness.send(t, `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"_meta":{"progressToken":"same"},"name":"a","arguments":{}}}`)
	_ = nextLine(t, childInput)
	harness.send(t, `{"jsonrpc":"2.0","id":"b","method":"tools/call","params":{"_meta":{"progressToken":"same"},"name":"b","arguments":{}}}`)
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"b"`) || !strings.Contains(got, "duplicate active progress token") {
		t.Fatalf("duplicate token error = %s", got)
	}
	assertNoLine(t, childInput, 100*time.Millisecond)
	harness.closeAndWait(t)
}

func TestRunChildWriteClassificationControlsReplay(t *testing.T) {
	first := newFaultChild()
	second := newFaultChild()
	third := newTestChild()
	firstInput := streamLines(first.inputReader)
	secondInput := streamLines(second.inputReader)
	thirdInput := streamLines(third.inputReader)
	children := []Child{first, second, third}
	startCount := 0
	var startMu sync.Mutex
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			startMu.Lock()
			defer startMu.Unlock()
			if startCount >= len(children) {
				return StartResult{}, errors.New("unexpected extra start")
			}
			child := children[startCount]
			startCount++
			return StartResult{Child: child, Actual: EngineRef{ID: "engine"}}, nil
		},
		RetryDelay: 10 * time.Millisecond,
	})
	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, firstInput)
	if err := first.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"first","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, firstInput)

	first.writer.arm(0, errors.New("not written"))
	harness.send(t, `{"jsonrpc":"2.0","id":"retry","method":"tools/list"}`)
	if got := nextLine(t, secondInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("second replay initialize = %s", got)
	}
	if err := second.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"second","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, secondInput)
	if got := nextLine(t, secondInput); !strings.Contains(got, `"id":"retry"`) {
		t.Fatalf("not-written request was not retained: %s", got)
	}
	assertNoLine(t, harness.hostOutput, 100*time.Millisecond)
	if err := second.write(`{"jsonrpc":"2.0","id":"retry","result":{"tools":[]}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)

	second.writer.arm(1, io.ErrUnexpectedEOF)
	harness.send(t, `{"jsonrpc":"2.0","id":"ambiguous","method":"tools/list"}`)
	if got := nextLine(t, harness.hostOutput); !strings.Contains(got, `"id":"ambiguous"`) || !strings.Contains(got, lostRequestMessage) {
		t.Fatalf("ambiguous lost error = %s", got)
	}
	if got := nextLine(t, thirdInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("third replay initialize = %s", got)
	}
	if err := third.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"third","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, thirdInput)
	assertNoLine(t, thirdInput, 100*time.Millisecond)
	harness.closeAndWait(t)
}

func TestRunReconcilesActiveEngineIdentity(t *testing.T) {
	first := newTestChild()
	second := newTestChild()
	firstInput := streamLines(first.inputReader)
	secondInput := streamLines(second.inputReader)
	firstAdmission := newTestAdmission(true)
	var desiredMu sync.Mutex
	desired := "engine-a"
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) {
			desiredMu.Lock()
			defer desiredMu.Unlock()
			return EngineRef{ID: desired}, nil
		},
		Start: func(_ context.Context, requested EngineRef) (StartResult, error) {
			switch requested.ID {
			case "engine-a":
				return StartResult{Child: first, Actual: requested, Admission: firstAdmission}, nil
			case "engine-b":
				select {
				case <-first.stopped:
				default:
					return StartResult{}, errors.New("engine B started before A stopped")
				}
				select {
				case <-firstAdmission.closed:
				default:
					return StartResult{}, errors.New("engine B started before A admission closed")
				}
				return StartResult{Child: second, Actual: requested}, nil
			default:
				return StartResult{}, errors.New("unknown engine")
			}
		},
		ReconcileInterval: 10 * time.Millisecond,
		RetryDelay:        10 * time.Millisecond,
	})
	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, firstInput)
	if err := first.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"a","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, firstInput)
	desiredMu.Lock()
	desired = "engine-b"
	desiredMu.Unlock()

	if got := nextLine(t, secondInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("engine B replay initialize = %s", got)
	}
	if err := second.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"b","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, secondInput)
	harness.send(t, `{"jsonrpc":"2.0","id":"on-b","method":"tools/list"}`)
	if got := nextLine(t, secondInput); !strings.Contains(got, `"id":"on-b"`) {
		t.Fatalf("engine B request = %s", got)
	}
	if err := second.write(`{"jsonrpc":"2.0","id":"on-b","result":{"tools":[]}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.closeAndWait(t)
}

func TestRunKeepsFallbackStableUntilRequestedIdentityChanges(t *testing.T) {
	fallback := newTestChild()
	replacement := newTestChild()
	fallbackInput := streamLines(fallback.inputReader)
	replacementInput := streamLines(replacement.inputReader)
	var stateMu sync.Mutex
	desired := EngineRef{ID: "engine-a"}
	starts := 0
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) {
			stateMu.Lock()
			defer stateMu.Unlock()
			return desired, nil
		},
		Start: func(_ context.Context, requested EngineRef) (StartResult, error) {
			stateMu.Lock()
			starts++
			call := starts
			stateMu.Unlock()
			switch {
			case call == 1 && requested.ID == "engine-a":
				return StartResult{Child: fallback, Actual: EngineRef{ID: "stable-launcher"}, Fallback: true}, nil
			case requested.ID == "engine-b":
				select {
				case <-fallback.stopped:
				default:
					return StartResult{}, errors.New("replacement started before fallback stopped")
				}
				return StartResult{Child: replacement, Actual: requested}, nil
			default:
				return StartResult{}, errors.New("fallback was retried without a requested identity change")
			}
		},
		ReconcileInterval: 10 * time.Millisecond,
		RetryDelay:        10 * time.Millisecond,
	})
	harness.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = nextLine(t, fallbackInput)
	if err := fallback.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"fallback","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	_ = nextLine(t, harness.hostOutput)
	harness.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_ = nextLine(t, fallbackInput)

	time.Sleep(100 * time.Millisecond)
	select {
	case <-fallback.stopped:
		t.Fatal("healthy fallback was replaced while requested identity was unchanged")
	default:
	}
	stateMu.Lock()
	if starts != 1 {
		t.Fatalf("start attempts while fallback was stable = %d, want 1", starts)
	}
	desired = EngineRef{ID: "engine-b"}
	stateMu.Unlock()

	if got := nextLine(t, replacementInput); !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("replacement replay initialize = %s", got)
	}
	if err := replacement.write(`{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"replacement","version":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, replacementInput); !strings.Contains(got, "notifications/initialized") {
		t.Fatalf("replacement initialized = %s", got)
	}
	harness.closeAndWait(t)
}

func TestRunHostEOFCancelsBlockedResolve(t *testing.T) {
	resolveCanceled := make(chan struct{})
	harness := startHarness(t, Config{
		Resolve: func(ctx context.Context) (EngineRef, error) {
			<-ctx.Done()
			close(resolveCanceled)
			return EngineRef{}, ctx.Err()
		},
		Start: func(context.Context, EngineRef) (StartResult, error) {
			t.Fatal("Start called after blocked resolve")
			return StartResult{}, nil
		},
	})
	harness.closeAndWait(t)
	select {
	case <-resolveCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked resolve did not observe host EOF cancellation")
	}
}

type discardWriteCloser struct{}

func (discardWriteCloser) Write(buffer []byte) (int, error) { return len(buffer), nil }
func (discardWriteCloser) Close() error                     { return nil }

type noProofChild struct {
	wait chan Exit
}

func (child *noProofChild) Stdin() io.WriteCloser      { return discardWriteCloser{} }
func (child *noProofChild) Stdout() io.ReadCloser      { return io.NopCloser(strings.NewReader("")) }
func (child *noProofChild) Wait() <-chan Exit          { return child.wait }
func (child *noProofChild) Stop(context.Context) error { return errors.New("stop failed") }

func TestRunFailsClosedWithoutChildTerminalProof(t *testing.T) {
	hostReader, hostWriter := io.Pipe()
	defer hostWriter.Close()
	starts := 0
	err := Run(context.Background(), Config{
		HostIn:  hostReader,
		HostOut: io.Discard,
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "bad"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			starts++
			return StartResult{Child: &noProofChild{wait: make(chan Exit)}, Actual: EngineRef{ID: "bad"}}, nil
		},
		GracefulStopTimeout: 10 * time.Millisecond,
		OutputDrainTimeout:  10 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "terminal proof") {
		t.Fatalf("Run error = %v", err)
	}
	if starts != 1 {
		t.Fatalf("successor started without proof; starts=%d", starts)
	}
}

type erroredTerminalChild struct {
	stdout       *io.PipeReader
	outputWriter *io.PipeWriter
	wait         chan Exit
	once         sync.Once
}

func newErroredTerminalChild() *erroredTerminalChild {
	stdout, outputWriter := io.Pipe()
	return &erroredTerminalChild{stdout: stdout, outputWriter: outputWriter, wait: make(chan Exit, 1)}
}

func (child *erroredTerminalChild) Stdin() io.WriteCloser { return discardWriteCloser{} }
func (child *erroredTerminalChild) Stdout() io.ReadCloser { return child.stdout }
func (child *erroredTerminalChild) Wait() <-chan Exit     { return child.wait }
func (child *erroredTerminalChild) Stop(context.Context) error {
	child.once.Do(func() {
		_ = child.outputWriter.Close()
		child.wait <- Exit{Code: 0}
		close(child.wait)
	})
	return errors.New("tree retirement failed")
}

func TestRunRejectsStopErrorEvenWithTerminalExit(t *testing.T) {
	hostReader, hostWriter := io.Pipe()
	child := newErroredTerminalChild()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Config{
			HostIn:  hostReader,
			HostOut: io.Discard,
			Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "bad-proof"}, nil },
			Start: func(context.Context, EngineRef) (StartResult, error) {
				close(started)
				return StartResult{Child: child, Actual: EngineRef{ID: "bad-proof"}}, nil
			},
			GracefulStopTimeout: 50 * time.Millisecond,
			OutputDrainTimeout:  50 * time.Millisecond,
		})
	}()
	<-started
	_ = hostWriter.Close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "terminal proof") {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run accepted an errored Stop as terminal proof")
	}
}

func TestRunHostPartialWriteIsTerminal(t *testing.T) {
	hostReader, hostWriter := io.Pipe()
	child := newTestChild()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Config{
			HostIn:  hostReader,
			HostOut: resultWriter{n: 1},
			Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
			Start: func(context.Context, EngineRef) (StartResult, error) {
				return StartResult{Child: child, Actual: EngineRef{ID: "engine"}}, nil
			},
		})
	}()
	if _, err := io.WriteString(hostWriter, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "host output") {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not terminate after partial host write")
	}
	_ = hostWriter.Close()
}

type closedWaitChild struct {
	stdout       *io.PipeReader
	outputWriter *io.PipeWriter
	wait         chan Exit
	stopped      chan struct{}
	once         sync.Once
}

func newClosedWaitChild() *closedWaitChild {
	stdout, outputWriter := io.Pipe()
	wait := make(chan Exit)
	close(wait)
	return &closedWaitChild{stdout: stdout, outputWriter: outputWriter, wait: wait, stopped: make(chan struct{})}
}

func (child *closedWaitChild) Stdin() io.WriteCloser { return discardWriteCloser{} }
func (child *closedWaitChild) Stdout() io.ReadCloser { return child.stdout }
func (child *closedWaitChild) Wait() <-chan Exit     { return child.wait }
func (child *closedWaitChild) Stop(context.Context) error {
	child.once.Do(func() {
		_ = child.outputWriter.Close()
		close(child.stopped)
	})
	return errors.New("closed wait has no terminal proof")
}

func TestRunRejectsClosedWaitWithoutTerminalResult(t *testing.T) {
	hostReader, hostWriter := io.Pipe()
	defer hostWriter.Close()
	child := newClosedWaitChild()
	starts := 0
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Config{
			HostIn:  hostReader,
			HostOut: io.Discard,
			Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "bad"}, nil },
			Start: func(context.Context, EngineRef) (StartResult, error) {
				starts++
				return StartResult{Child: child, Actual: EngineRef{ID: "bad"}}, nil
			},
			GracefulStopTimeout: 20 * time.Millisecond,
			OutputDrainTimeout:  20 * time.Millisecond,
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed without a terminal result") {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not fail on a closed Wait channel")
	}
	if starts != 1 {
		t.Fatalf("successor started without terminal proof; starts=%d", starts)
	}
	select {
	case <-child.stopped:
	default:
		t.Fatal("invalid child was not asked to stop")
	}
}

func TestRunStartTimeoutTerminatesWithoutConcurrentRetry(t *testing.T) {
	starts := 0
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "slow"}, nil },
		Start: func(ctx context.Context, _ EngineRef) (StartResult, error) {
			starts++
			<-ctx.Done()
			return StartResult{}, ctx.Err()
		},
		StartTimeout:        20 * time.Millisecond,
		GracefulStopTimeout: 50 * time.Millisecond,
	})
	select {
	case err := <-harness.done:
		if err == nil || !strings.Contains(err.Error(), "resolve/start attempt timed out") {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not enforce the start timeout")
	}
	if starts != 1 {
		t.Fatalf("start attempts = %d, want one bounded authority attempt", starts)
	}
}

func TestRunUnprovenStartRollbackTerminatesWithoutRetry(t *testing.T) {
	starts := 0
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "unsafe"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			starts++
			return StartResult{}, ErrStartRollbackUnproven
		},
		RetryDelay: 5 * time.Millisecond,
	})
	defer harness.hostInput.Close()

	select {
	case err := <-harness.done:
		if !errors.Is(err, ErrStartRollbackUnproven) {
			t.Fatalf("Run error = %v, want rollback-unproven sentinel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run retried after unproven start rollback")
	}
	if starts != 1 {
		t.Fatalf("start attempts = %d, want one fail-closed attempt", starts)
	}
}

func TestRunCleansReturnedStartAuthorityBeforeRetry(t *testing.T) {
	first := newTestChild()
	second := newTestChild()
	secondStarted := make(chan struct{})
	starts := 0
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			starts++
			if starts == 1 {
				return StartResult{Child: first, Actual: EngineRef{ID: "engine"}}, ErrStartRollbackUnproven
			}
			select {
			case <-first.stopped:
			default:
				t.Error("retry started before returned authority was retired")
			}
			close(secondStarted)
			return StartResult{Child: second, Actual: EngineRef{ID: "engine"}}, nil
		},
		RetryDelay: 5 * time.Millisecond,
	})

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not retry after proving returned authority retired")
	}
	harness.closeAndWait(t)
	if starts != 2 {
		t.Fatalf("start attempts = %d, want one cleanup then one retry", starts)
	}
}

func TestRunCleansLateStartResultBeforeReturning(t *testing.T) {
	hostReader, hostWriter := io.Pipe()

	child := newTestChild()
	admission := newTestAdmission(true)
	startEntered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Config{
			HostIn:  hostReader,
			HostOut: io.Discard,
			Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "late"}, nil },
			Start: func(ctx context.Context, _ EngineRef) (StartResult, error) {
				close(startEntered)
				<-ctx.Done()
				return StartResult{Child: child, Actual: EngineRef{ID: "late"}, Admission: admission}, nil
			},
		})
	}()
	select {
	case <-startEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("Start was not entered")
	}
	if err := hostWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not await late start cleanup")
	}
	select {
	case <-child.stopped:
	default:
		t.Fatal("late child was not stopped before Run returned")
	}
	select {
	case <-admission.closed:
	default:
		t.Fatal("late admission was not closed before Run returned")
	}
}

func TestStartErrorRollsBackReturnedAuthorityBeforeRetry(t *testing.T) {
	first := newTestChild()
	firstAdmission := newTestAdmission(true)
	second := newTestChild()
	secondStarted := make(chan struct{})
	starts := 0
	harness := startHarness(t, Config{
		Resolve: func(context.Context) (EngineRef, error) { return EngineRef{ID: "engine"}, nil },
		Start: func(context.Context, EngineRef) (StartResult, error) {
			starts++
			if starts == 1 {
				return StartResult{Child: first, Actual: EngineRef{ID: "engine"}, Admission: firstAdmission}, errors.New("start failed after allocation")
			}
			select {
			case <-first.stopped:
			default:
				return StartResult{}, errors.New("retry preceded child rollback")
			}
			select {
			case <-firstAdmission.closed:
			default:
				return StartResult{}, errors.New("retry preceded admission rollback")
			}
			close(secondStarted)
			return StartResult{Child: second, Actual: EngineRef{ID: "engine"}}, nil
		},
		RetryDelay: 10 * time.Millisecond,
	})
	select {
	case <-secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("clean retry did not start")
	}
	harness.closeAndWait(t)
}

func TestDormancyCommitCannotBeatReaderObservedDemand(t *testing.T) {
	exitDone := make(chan struct{})
	close(exitDone)
	pumpDone := make(chan struct{})
	close(pumpDone)
	reapDone := make(chan struct{})
	close(reapDone)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &runner{
		ctx:      ctx,
		cancel:   cancel,
		cfg:      Config{GracefulStopTimeout: time.Second, OutputDrainTimeout: time.Second, runtimeClock: realClock{}},
		events:   make(chan any),
		state:    StateAwaitingDormantExit,
		terminal: true,
		host:     newDirectionRegistry(),
		child:    newDirectionRegistry(),
		current: &generation{
			id:        1,
			child:     &noProofChild{wait: make(chan Exit)},
			stdin:     discardWriteCloser{},
			stdout:    io.NopCloser(strings.NewReader("")),
			cancel:    func() {},
			pumpDone:  pumpDone,
			reapDone:  reapDone,
			exitDone:  exitDone,
			exit:      Exit{Code: ProtocolV2().DormantExitCode()},
			outputEOF: true,
		},
	}
	runner.hostBoundary.record(time.Now())
	runner.maybeCompleteDormancy()
	if runner.state == StateDormant {
		t.Fatal("reader-observed demand lost the dormancy race")
	}
	if runner.current != nil {
		t.Fatal("terminal child was not finalized")
	}
}

func TestUnknownQuiescingCancellationIsNotQueued(t *testing.T) {
	frame, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"unknown"}}`), 1024)
	if err != nil || frame.utilityErr != nil {
		t.Fatalf("cancellation parse=%v utility=%v", err, frame.utilityErr)
	}
	runner := &runner{
		state:   StateQuiescing,
		current: &generation{id: 1},
		host:    newDirectionRegistry(),
		child:   newDirectionRegistry(),
	}
	runner.handleHostCancellation(&pendingFrame{frame: frame})
	if runner.pending.len() != 0 {
		t.Fatal("unknown cancellation consumed pending capacity")
	}
}

func TestDormantAckCommitsQueuedCancellationWithoutLostError(t *testing.T) {
	request, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":"cancel-me","method":"tools/list"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	cancellation, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel-me"}}`), 1024)
	if err != nil || cancellation.utilityErr != nil {
		t.Fatalf("cancellation parse=%v utility=%v", err, cancellation.utilityErr)
	}
	runner := &runner{
		state:   StateQuiescing,
		current: &generation{id: 1},
		host:    newDirectionRegistry(),
		child:   newDirectionRegistry(),
	}
	runner.host.register(request, 1)
	item := &pendingFrame{frame: cancellation, sameGenerationOnly: true}
	if !runner.pending.enqueue(item, 4, 4096) {
		t.Fatal("failed to queue cancellation")
	}
	runner.commitQuiescingCancellations()
	if _, exists := runner.host.requests[request.id.key]; exists {
		t.Fatal("ACK-bound cancellation left request eligible for a lost error")
	}
	if !runner.host.tombstones.contains(tombstoneRequest, request.id.key) {
		t.Fatal("ACK-bound cancellation did not tombstone request")
	}
}

func TestReplaceCurrentCommitsQuiescingCancellationBeforeLostErrors(t *testing.T) {
	request, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":"cancel-me","method":"tools/list"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	cancellation, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel-me"}}`), 1024)
	if err != nil || cancellation.utilityErr != nil {
		t.Fatalf("cancellation parse=%v utility=%v", err, cancellation.utilityErr)
	}
	exitDone := make(chan struct{})
	close(exitDone)
	pumpDone := make(chan struct{})
	close(pumpDone)
	reapDone := make(chan struct{})
	close(reapDone)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	runner := &runner{
		ctx:    ctx,
		cancel: cancel,
		cfg: Config{
			HostOut:             &output,
			Resolve:             func(context.Context) (EngineRef, error) { return EngineRef{}, errors.New("stop") },
			Start:               func(context.Context, EngineRef) (StartResult, error) { return StartResult{}, errors.New("unused") },
			StartTimeout:        time.Second,
			GracefulStopTimeout: time.Second,
			OutputDrainTimeout:  time.Second,
			runtimeClock:        realClock{},
		},
		events: make(chan any, 1),
		state:  StateQuiescing,
		host:   newDirectionRegistry(),
		child:  newDirectionRegistry(),
		current: &generation{
			id:       1,
			stdin:    discardWriteCloser{},
			cancel:   func() {},
			pumpDone: pumpDone,
			reapDone: reapDone,
			exitDone: exitDone,
			exit:     Exit{},
		},
	}
	runner.host.register(request, 1)
	if !runner.pending.enqueue(&pendingFrame{frame: cancellation, sameGenerationOnly: true}, 4, 4096) {
		t.Fatal("failed to queue cancellation")
	}
	runner.replaceCurrent(ReasonCrash)
	if strings.Contains(output.String(), lostRequestMessage) {
		t.Fatalf("canceled request received a lost error: %s", output.String())
	}
	if _, exists := runner.host.requests[request.id.key]; exists {
		t.Fatal("replacement left canceled request active")
	}
	if !runner.host.tombstones.contains(tombstoneRequest, request.id.key) {
		t.Fatal("replacement did not tombstone canceled request")
	}
	runner.stopTimer(&runner.startTimer)
	cancel()
	runner.background.Wait()
}

func TestAwaitingDormantExitForwardsOnlyFirstOrdinaryFrame(t *testing.T) {
	first, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"first"}}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"second"}}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	runner := &runner{
		cfg:     Config{HostOut: &output},
		state:   StateAwaitingDormantExit,
		current: &generation{id: 1},
		host:    newDirectionRegistry(),
		child:   newDirectionRegistry(),
	}
	runner.handleChildFrame(first)
	runner.handleChildFrame(second)
	got := output.String()
	if !strings.Contains(got, `"data":"first"`) {
		t.Fatalf("first post-ACK frame was not forwarded: %s", got)
	}
	if strings.Contains(got, `"data":"second"`) {
		t.Fatalf("second post-ACK frame was forwarded: %s", got)
	}
}

func TestDormantLeaseExpiresAfterNonWakingPreDeadlineFrame(t *testing.T) {
	frame, err := parseFrame(ProtocolV2().Frame(ControlDormantReady), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deadline := time.Now().Add(-time.Millisecond)
	arrived := deadline.Add(-time.Millisecond)
	runner := &runner{
		ctx:    ctx,
		cancel: cancel,
		cfg: Config{
			DormantLease: time.Second,
			runtimeClock: realClock{},
		},
		state:                StateDormant,
		host:                 newDirectionRegistry(),
		child:                newDirectionRegistry(),
		dormantLeaseDeadline: deadline,
		dormantLeaseTimer:    realClock{}.NewTimer(time.Hour),
	}
	sequence := runner.hostBoundary.record(arrived)
	runner.handleDormantLease(deadline)
	if runner.terminal {
		t.Fatal("lease beat a reader-observed pre-deadline frame")
	}
	runner.handleHostEvent(hostFrameEvent{sequence: sequence, arrived: arrived, frame: frame})
	if !runner.terminal {
		t.Fatal("non-waking frame permanently disarmed dormant lease expiry")
	}
}

func TestSafeToCommitDormantRequiresNoLiveOrUnreadWork(t *testing.T) {
	newRunner := func() *runner {
		return &runner{
			current: &generation{id: 1},
			host:    newDirectionRegistry(),
			child:   newDirectionRegistry(),
		}
	}
	clean := newRunner()
	if !clean.safeToCommitDormant(0) {
		t.Fatal("clean initialized generation was not eligible for dormant commit")
	}

	withRequest := newRunner()
	request, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	withRequest.host.register(request, 1)
	if withRequest.safeToCommitDormant(0) {
		t.Fatal("active request crossed dormant gate")
	}

	withTask := newRunner()
	withTask.host.tasks["task-1"] = &taskRecord{id: "task-1", generation: 1}
	if withTask.safeToCommitDormant(0) {
		t.Fatal("active task crossed dormant gate")
	}

	withToken := newRunner()
	withToken.child.tokens[scalarKey{kind: scalarString, value: "token"}] = &tokenRecord{generation: 1}
	if withToken.safeToCommitDormant(0) {
		t.Fatal("active progress token crossed dormant gate")
	}

	withPending := newRunner()
	withPending.pending.items = append(withPending.pending.items, &pendingFrame{frame: request})
	if withPending.safeToCommitDormant(0) {
		t.Fatal("pending host frame crossed dormant gate")
	}

	withUnreadBoundary := newRunner()
	observed := withUnreadBoundary.hostBoundary.record(time.Now())
	if withUnreadBoundary.safeToCommitDormant(observed) {
		t.Fatal("reader-observed unprocessed host demand crossed dormant gate")
	}
}
