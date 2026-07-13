package owner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResilientClient_IdleSuspendsAndReconnectsOnDemand(t *testing.T) {
	path := newTestIPCPath(t)
	srv, _ := startEchoIPCServer(t, path)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()
	var refreshCalls atomic.Int32

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:            ccStdinR,
		Stdout:           ccStdoutW,
		InitialIPCPath:   path,
		IdleSuspendDelay: 100 * time.Millisecond,
		RefreshToken: func() (string, string, error) {
			refreshCalls.Add(1)
			return path, "", nil
		},
		Reconnect: func() (string, string, error) {
			return path, "", nil
		},
		Logger: resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() { errCh <- RunResilientClient(cfg) }()

	outLines := make(chan []byte, 100)
	go func() {
		scanner := bufio.NewScanner(ccStdoutR)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			outLines <- line
		}
		close(outLines)
	}()
	if _, err := io.WriteString(ccStdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	waitForResponseID(t, outLines, 1)

	select {
	case <-srv.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("idle shim did not close its IPC session")
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh called while suspended without demand: %d", got)
	}

	if _, err := io.WriteString(ccStdinW, `{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`+"\n"); err != nil {
		t.Fatalf("write wake request: %v", err)
	}
	waitForResponseID(t, outLines, 2)
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls after wake = %d, want 1", got)
	}

	_ = ccStdinW.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunResilientClient: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resilient client did not exit after stdin EOF")
	}
}

func TestResilientClient_IdleSuspendGateDenialKeepsIPCConnected(t *testing.T) {
	path := newTestIPCPath(t)
	srv, _ := startEchoIPCServer(t, path)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()
	var gateCalls atomic.Int32

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:            ccStdinR,
		Stdout:           ccStdoutW,
		InitialIPCPath:   path,
		IdleSuspendDelay: 50 * time.Millisecond,
		IdleSuspendGate: func() (bool, string, error) {
			gateCalls.Add(1)
			return false, "owner busy", nil
		},
		Logger: resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() { errCh <- RunResilientClient(cfg) }()

	outLines := make(chan []byte, 100)
	go func() {
		scanner := bufio.NewScanner(ccStdoutR)
		for scanner.Scan() {
			outLines <- append([]byte(nil), scanner.Bytes()...)
		}
		close(outLines)
	}()

	if _, err := io.WriteString(ccStdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	waitForResponseID(t, outLines, 1)

	deadline := time.Now().Add(2 * time.Second)
	for gateCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gateCalls.Load() == 0 {
		t.Fatal("idle suspend gate was not consulted")
	}
	select {
	case <-srv.closed:
		t.Fatal("IPC session closed despite idle suspend gate denial")
	case <-time.After(150 * time.Millisecond):
	}

	_ = ccStdinW.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunResilientClient: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resilient client did not exit after stdin EOF")
	}
}

func TestResilientClient_ActivityDuringIdleGateDefersSuspend(t *testing.T) {
	path := newTestIPCPath(t)
	srv, _ := startEchoIPCServer(t, path)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()
	gateEntered := make(chan struct{})
	gateRelease := make(chan struct{})
	var gateCalls atomic.Int32

	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:            ccStdinR,
		Stdout:           ccStdoutW,
		InitialIPCPath:   path,
		IdleSuspendDelay: 200 * time.Millisecond,
		IdleSuspendGate: func() (bool, string, error) {
			if gateCalls.Add(1) == 1 {
				close(gateEntered)
				<-gateRelease
				return true, "", nil
			}
			return false, "test complete", nil
		},
		Logger: resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() { errCh <- RunResilientClient(cfg) }()

	outLines := make(chan []byte, 100)
	go func() {
		scanner := bufio.NewScanner(ccStdoutR)
		for scanner.Scan() {
			outLines <- append([]byte(nil), scanner.Bytes()...)
		}
		close(outLines)
	}()

	if _, err := io.WriteString(ccStdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	waitForResponseID(t, outLines, 1)

	select {
	case <-gateEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("idle suspend gate was not entered")
	}

	if _, err := io.WriteString(ccStdinW, `{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`+"\n"); err != nil {
		t.Fatalf("write activity during gate: %v", err)
	}
	waitForResponseID(t, outLines, 2)
	close(gateRelease)

	select {
	case <-srv.closed:
		t.Fatal("IPC session suspended immediately despite activity during the gate")
	case <-time.After(100 * time.Millisecond):
	}

	_ = ccStdinW.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunResilientClient: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resilient client did not exit after stdin EOF")
	}
}

func TestResilientClient_IdleDormantHandshake(t *testing.T) {
	path := newTestIPCPath(t)
	startEchoIPCServer(t, path)

	ccStdinR, ccStdinW := io.Pipe()
	ccStdoutR, ccStdoutW := io.Pipe()
	cfg := ResilientClientConfig{
		ProbeGracePeriod: time.Nanosecond,
		Stdin:            ccStdinR,
		Stdout:           ccStdoutW,
		InitialIPCPath:   path,
		IdleSuspendDelay: 50 * time.Millisecond,
		IdleDormantGrace: 50 * time.Millisecond,
		Logger:           resilientTestLogger(t),
	}

	errCh := make(chan error, 1)
	go func() { errCh <- RunResilientClient(cfg) }()

	outLines := make(chan []byte, 100)
	go func() {
		scanner := bufio.NewScanner(ccStdoutR)
		for scanner.Scan() {
			outLines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()

	if _, err := io.WriteString(ccStdinW, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	waitForResponseID(t, outLines, 1)
	waitForNotificationMethod(t, outLines, resilientDormantReadyMethod)

	if _, err := io.WriteString(ccStdinW, fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"method\":%q}\n", resilientDormantCommitMethod)); err != nil {
		t.Fatalf("write dormant commit: %v", err)
	}
	waitForNotificationMethod(t, outLines, resilientDormantAckMethod)

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrIdleDormant) {
			t.Fatalf("RunResilientClient error = %v, want ErrIdleDormant", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resilient client did not complete dormant handshake")
	}
}

type blockingResilientWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingResilientWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

type splitResilientReadWriter struct {
	io.Reader
	written bytes.Buffer
}

type dormantReadyWriter struct {
	bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *dormantReadyWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(resilientDormantReadyMethod)) {
		w.once.Do(func() { close(w.ready) })
	}
	return w.Buffer.Write(p)
}

func (rw *splitResilientReadWriter) Write(p []byte) (int, error) {
	return rw.written.Write(p)
}

func TestResilientClient_WriterOwnedFrameBlocksIdleSuspend(t *testing.T) {
	cases := []struct {
		name               string
		frame              string
		wantIdleAfterWrite bool
	}{
		{name: "notification", frame: `{"jsonrpc":"2.0","method":"notifications/test"}`, wantIdleAfterWrite: true},
		{name: "request", frame: `{"jsonrpc":"2.0","id":7,"method":"tools/call"}`, wantIdleAfterWrite: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := &resilientClient{
				cfg:        ResilientClientConfig{IdleSuspendDelay: time.Nanosecond},
				msgFromCC:  make(chan []byte, 1),
				msgFromIPC: make(chan []byte, 1),
				log:        resilientTestLogger(t),
			}
			rc.initialized.Store(true)
			rc.effectiveIdleDelay.Store(int64(time.Nanosecond))
			rc.lastHostActivity.Store(time.Now().Add(-time.Second).UnixNano())
			rc.localWork.Add(1)
			rc.msgFromCC <- []byte(tc.frame)

			writer := &blockingResilientWriter{entered: make(chan struct{}), release: make(chan struct{})}
			ipcEOF := make(chan struct{})
			done := make(chan []byte, 1)
			go rc.runIPCWriter(writer, ipcEOF, done)
			select {
			case <-writer.entered:
			case <-time.After(time.Second):
				t.Fatal("IPC writer did not take frame")
			}
			if rc.shouldIdleSuspend() {
				t.Fatal("writer-owned frame was misclassified as idle")
			}

			close(writer.release)
			deadline := time.Now().Add(time.Second)
			for rc.localWork.Load() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := rc.localWork.Load(); got != 0 {
				t.Fatalf("local work after completed write = %d, want 0", got)
			}
			if got := rc.shouldIdleSuspend(); got != tc.wantIdleAfterWrite {
				t.Fatalf("shouldIdleSuspend after write = %v, want %v", got, tc.wantIdleAfterWrite)
			}
			close(ipcEOF)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("IPC writer did not stop")
			}
		})
	}
}

func TestResilientClient_SuspendBarrierDefersNewHostFrame(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":91,"method":"tools/call"}`)
	for _, source := range []string{"stdin", "inject"} {
		t.Run(source, func(t *testing.T) {
			rc := &resilientClient{
				cfg: ResilientClientConfig{
					IdleSuspendDelay: time.Nanosecond,
					Stdin:            strings.NewReader(string(frame) + "\n"),
				},
				msgFromCC:  make(chan []byte, 1),
				msgFromIPC: make(chan []byte, 1),
				log:        resilientTestLogger(t),
			}
			rc.initialized.Store(true)
			rc.effectiveIdleDelay.Store(int64(time.Nanosecond))
			rc.lastHostActivity.Store(time.Now().Add(-time.Second).UnixNano())
			if !rc.beginIdleSuspend() {
				t.Fatal("idle suspend barrier was not acquired")
			}

			var oldIPC bytes.Buffer
			ipcEOF := make(chan struct{})
			writerDone := make(chan []byte, 1)
			go rc.runIPCWriter(&oldIPC, ipcEOF, writerDone)
			if source == "inject" {
				if err := rc.injectFrame(frame); err != nil {
					t.Fatalf("injectFrame: %v", err)
				}
			} else {
				stdinDone := make(chan error, 1)
				go rc.runStdinReader(stdinDone)
			}

			select {
			case deferred := <-writerDone:
				if !bytes.Equal(deferred, frame) {
					t.Fatalf("deferred frame = %s, want %s", deferred, frame)
				}
			case <-time.After(time.Second):
				t.Fatal("writer did not defer frame at suspend barrier")
			}
			if oldIPC.Len() != 0 {
				t.Fatalf("post-barrier frame reached old IPC: %s", oldIPC.String())
			}
			if got := rc.localWork.Load(); got != 1 {
				t.Fatalf("deferred local work = %d, want 1 until wake write", got)
			}
			rc.localWork.Add(-1)
		})
	}
}

func TestResilientClient_DemandBeforeCommitDropsPrivateCommit(t *testing.T) {
	var host bytes.Buffer
	rc := &resilientClient{
		cfg:        ResilientClientConfig{Stdout: &host},
		msgFromCC:  make(chan []byte, 2),
		stdoutDead: make(chan struct{}),
		log:        resilientTestLogger(t),
	}
	demandFrame := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call"}`)
	commitFrame := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":%q}`, resilientDormantCommitMethod))
	rc.localWork.Add(2)
	rc.msgFromCC <- demandFrame
	rc.msgFromCC <- commitFrame

	demand, committed, err := rc.negotiateDormant(&sync.Mutex{}, make(chan error))
	if err != nil {
		t.Fatalf("negotiateDormant: %v", err)
	}
	if committed || !bytes.Equal(demand, demandFrame) {
		t.Fatalf("dormant negotiation = (%s, %v), want host demand and NACK", demand, committed)
	}

	var upstream bytes.Buffer
	if err := rc.forwardToIPC(&upstream, demand); err != nil {
		t.Fatalf("forward demand: %v", err)
	}
	rc.localWork.Add(-1)
	ipcEOF := make(chan struct{})
	done := make(chan []byte, 1)
	go rc.runIPCWriter(&upstream, ipcEOF, done)
	deadline := time.Now().Add(time.Second)
	for rc.localWork.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(ipcEOF)
	<-done

	if got := rc.localWork.Load(); got != 0 {
		t.Fatalf("local work = %d, want 0", got)
	}
	if strings.Contains(upstream.String(), resilientDormantCommitMethod) {
		t.Fatalf("private dormant commit reached upstream: %s", upstream.String())
	}
	if got := strings.Count(upstream.String(), `"id":9`); got != 1 {
		t.Fatalf("host demand forwarded %d times, want once: %s", got, upstream.String())
	}
}

func TestResilientClient_InjectRaceDormantCommit(t *testing.T) {
	for i := 0; i < 100; i++ {
		host := &dormantReadyWriter{ready: make(chan struct{})}
		rc := &resilientClient{
			cfg:        ResilientClientConfig{Stdout: host},
			msgFromCC:  make(chan []byte, 4),
			stdoutDead: make(chan struct{}),
			log:        resilientTestLogger(t),
		}
		result := make(chan struct {
			demand    []byte
			committed bool
			err       error
		}, 1)
		go func() {
			demand, committed, err := rc.negotiateDormant(&sync.Mutex{}, make(chan error))
			result <- struct {
				demand    []byte
				committed bool
				err       error
			}{demand, committed, err}
		}()
		<-host.ready

		commit := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":%q}`, resilientDormantCommitMethod))
		frame := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call"}`, i+100))
		start := make(chan struct{})
		injectErr := make(chan error, 1)
		go func() {
			<-start
			rc.localWork.Add(1)
			rc.msgFromCC <- commit
		}()
		go func() {
			<-start
			injectErr <- rc.injectFrame(frame)
		}()
		close(start)

		got := <-result
		if got.err != nil {
			t.Fatalf("iteration %d negotiateDormant: %v", i, got.err)
		}
		err := <-injectErr
		if got.committed {
			if !errors.Is(err, ErrInjectClosed) {
				t.Fatalf("iteration %d committed injection error = %v, want ErrInjectClosed", i, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("iteration %d NACK injection error = %v", i, err)
		}

		var upstream bytes.Buffer
		if err := rc.forwardToIPC(&upstream, got.demand); err != nil {
			t.Fatalf("iteration %d forward demand: %v", i, err)
		}
		rc.localWork.Add(-1)
		ipcEOF := make(chan struct{})
		done := make(chan []byte, 1)
		go rc.runIPCWriter(&upstream, ipcEOF, done)
		deadline := time.Now().Add(time.Second)
		for rc.localWork.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		close(ipcEOF)
		<-done
		if strings.Contains(upstream.String(), resilientDormantCommitMethod) {
			t.Fatalf("iteration %d private commit leaked upstream: %s", i, upstream.String())
		}
		if got := strings.Count(upstream.String(), fmt.Sprintf(`"id":%d`, i+100)); got != 1 {
			t.Fatalf("iteration %d injected frame forwarded %d times, want once: %s", i, got, upstream.String())
		}
	}
}

func TestResilientClient_QueuedInjectsKeepFIFOAcrossDormantNack(t *testing.T) {
	var host bytes.Buffer
	rc := &resilientClient{
		cfg:        ResilientClientConfig{Stdout: &host},
		msgFromCC:  make(chan []byte, 3),
		stdoutDead: make(chan struct{}),
		log:        resilientTestLogger(t),
	}
	commit := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":%q}`, resilientDormantCommitMethod))
	rc.localWork.Add(1)
	rc.msgFromCC <- commit
	if err := rc.injectFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if err := rc.injectFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"second"}`)); err != nil {
		t.Fatal(err)
	}

	demand, committed, err := rc.negotiateDormant(&sync.Mutex{}, make(chan error))
	if err != nil || committed {
		t.Fatalf("negotiateDormant = (%s, %v, %v), want NACK", demand, committed, err)
	}
	var upstream bytes.Buffer
	if err := rc.forwardToIPC(&upstream, demand); err != nil {
		t.Fatal(err)
	}
	rc.localWork.Add(-1)
	ipcEOF := make(chan struct{})
	done := make(chan []byte, 1)
	go rc.runIPCWriter(&upstream, ipcEOF, done)
	deadline := time.Now().Add(time.Second)
	for rc.localWork.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(ipcEOF)
	<-done
	got := upstream.String()
	if strings.Contains(got, resilientDormantCommitMethod) ||
		strings.Count(got, `"id":1`) != 1 || strings.Count(got, `"id":2`) != 1 ||
		strings.Index(got, `"id":1`) > strings.Index(got, `"id":2`) {
		t.Fatalf("FIFO or lifecycle filtering failed: %s", got)
	}
}

func TestResilientClient_ReplayInitWaitsForMatchingIDAndPreservesPrefix(t *testing.T) {
	notification := `{"jsonrpc":"2.0","method":"notifications/test"}`
	wrong := `{"jsonrpc":"2.0","id":2,"result":{"capabilities":{"x-mux":{"idleTimeout":999,"persistent":true}}}}`
	matching := `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"idleTimeout":7}}}}`
	conn := &splitResilientReadWriter{Reader: strings.NewReader(notification + "\n" + wrong + "\n" + matching + "\n")}
	rc := &resilientClient{
		msgFromIPC: make(chan []byte, 3),
		log:        resilientTestLogger(t),
	}
	rc.initCache.request = []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	rc.initCache.requestID = "1"
	rc.effectiveIdleDelay.Store(int64(time.Second))

	if err := rc.replayInit(conn); err != nil {
		t.Fatalf("replayInit: %v", err)
	}
	if !rc.initialized.Load() {
		t.Fatal("matching initialize response was not observed")
	}
	if got := time.Duration(rc.effectiveIdleDelay.Load()); got != 7*time.Second {
		t.Fatalf("effective idle delay = %s, want 7s from matching response", got)
	}
	if rc.persistent.Load() {
		t.Fatal("wrong-id persistent hint was applied")
	}
	if got := string(<-rc.msgFromIPC); got != notification {
		t.Fatalf("first preserved frame = %s, want %s", got, notification)
	}
	if got := string(<-rc.msgFromIPC); got != wrong {
		t.Fatalf("second preserved frame = %s, want %s", got, wrong)
	}
	select {
	case extra := <-rc.msgFromIPC:
		t.Fatalf("matching initialize response leaked to normal output: %s", extra)
	default:
	}
}

func TestResilientClient_InitializeHintsRequireMatchingID(t *testing.T) {
	rc := &resilientClient{}
	rc.initCache.requestID = "1"
	rc.effectiveIdleDelay.Store(int64(10 * time.Second))

	rc.observeIPCResponse([]byte(`{"jsonrpc":"2.0","id":2,"result":{"capabilities":{"x-mux":{"idleTimeout":99,"persistent":true}}}}`))
	if rc.initialized.Load() || rc.persistent.Load() || time.Duration(rc.effectiveIdleDelay.Load()) != 10*time.Second {
		t.Fatal("wrong-id response changed initialize lifecycle hints")
	}
	rc.observeIPCResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"x-mux":{"idleTimeout":3,"persistent":true}}}}`))
	if !rc.initialized.Load() || !rc.persistent.Load() {
		t.Fatal("matching response did not apply initialize lifecycle hints")
	}
	if got := time.Duration(rc.effectiveIdleDelay.Load()); got != 3*time.Second {
		t.Fatalf("effective idle delay = %s, want 3s", got)
	}
}

func TestResilientClient_ZeroIdleSuspendDelayDoesNotSuspend(t *testing.T) {
	path := newTestIPCPath(t)
	srv, _ := startEchoIPCServer(t, path)
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunResilientClient(ResilientClientConfig{
			ProbeGracePeriod: time.Nanosecond,
			Stdin:            stdinR,
			Stdout:           stdoutW,
			InitialIPCPath:   path,
			Logger:           resilientTestLogger(t),
		})
	}()

	lines := make(chan []byte, 2)
	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			lines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	_, _ = io.WriteString(stdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n")
	waitForResponseID(t, lines, 1)
	select {
	case <-srv.closed:
		t.Fatal("zero-value idle suspension closed IPC")
	case <-time.After(150 * time.Millisecond):
	}

	_ = stdinW.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunResilientClient: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resilient client did not exit")
	}
}

func waitForNotificationMethod(t *testing.T, lines <-chan []byte, want string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			var msg struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &msg) == nil && msg.Method == want {
				return
			}
		case <-timer.C:
			t.Fatalf("notification %q not observed", want)
		}
	}
}

func waitForResponseID(t *testing.T, lines <-chan []byte, want int) {
	t.Helper()
	type envelope struct {
		ID json.RawMessage `json:"id"`
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stdout closed before response id %d", want)
			}
			var msg envelope
			if json.Unmarshal(line, &msg) == nil && string(msg.ID) == strconv.Itoa(want) {
				return
			}
		case <-timer.C:
			t.Fatalf("response id %d not observed", want)
		}
	}
}
