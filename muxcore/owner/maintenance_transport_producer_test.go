package owner

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func TestMaintenanceTransportPlainReaderSettlement(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
			for _, held := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/era_%d/held_%t", mode, protocol, held), func(t *testing.T) {
					o, gate, _ := maintenanceAuthorizationOwner(t, mode, protocol, false, nil)
					if o.sessionHandler != nil || o.authorizeSession != nil || o.onFrameReceived != nil {
						t.Fatal("plain transport fixture unexpectedly has user hooks")
					}
					launch := o.CurrentLaunchContext()
					reader, writer := io.Pipe()
					t.Cleanup(func() { _ = writer.Close() })
					exit := &maintenanceReaderExit{ReadCloser: reader, exited: make(chan struct{})}
					s := NewSession(exit, &safeBuf{})
					s.Cwd = t.TempDir()
					s.Env = map[string]string{"TRANSPORT_CONTEXT": "retained"}
					const token = "cafebabe"
					if !o.PreRegisterInitial(token, s.Cwd, s.Env) || !o.sessionMgr.Bind(token, o.ServerID(), s) {
						t.Fatal("plain session did not establish real token history")
					}
					o.AddSession(s)
					o.launchContextMu.Lock()
					var unlockOnce sync.Once
					unlock := func() { unlockOnce.Do(o.launchContextMu.Unlock) }
					defer unlock()
					if held {
						gate.Lock()
						o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: time.Now()})
						gate.Unlock()
					}
					_, finalized, err := o.FinalizeForRemoval(false, 100*time.Millisecond)
					if finalized || err == nil {
						t.Fatalf("early completion before plain reader deferred settlement: finalized=%t err=%v", finalized, err)
					}
					select {
					case <-exit.exited:
					case <-time.After(time.Second):
						t.Fatal("teardown did not produce real pipe EOF/closed-reader return")
					}
					if !s.IsClosed() || o.SessionCount() != 1 || o.PendingRequests() != 0 || o.MaintenanceRetired() {
						t.Fatal("connection close erased a still-live plain reader or invented request work")
					}
					select {
					case <-o.Done():
						t.Fatal("OwnerDone closed before actual plain reader removal")
					default:
					}
					snapshot, sessions := o.ExportSnapshotState()
					if len(sessions) != 1 || sessions[0].MuxSessionID != s.MuxSessionID || len(snapshot.BoundTokens) != 1 || snapshot.BoundTokens[0].Token != token || snapshot.BoundTokens[0].Cwd != s.Cwd {
						t.Fatal("blocked retirement lost exact session/context/token history")
					}
					if o.protocolEra != protocol || !reflect.DeepEqual(launch, o.CurrentLaunchContext()) {
						t.Fatal("transport retirement changed the pinned era or launch context")
					}
					unlock()
					waitForCondition(t, time.Second, func() bool {
						_, proven, _ := o.FinalizeForRemoval(false, time.Second)
						return proven
					}, "actual plain reader settlement did not permit existing finalization")
					if !o.MaintenanceRetired() || o.SessionCount() != 0 || o.PendingRequests() != 0 || o.nativeWork.Load() != 0 {
						t.Fatal("plain producer retained authority after real settlement")
					}
				})
			}
		}
	}
}

// Wrap only the real IPC listener/connection, observing Accept and the second
// readToken read. No synthetic Accept, EOF, callback, or retirement proof is used.
type transportAdmissionListener struct {
	net.Listener
	accepted     chan *transportAdmissionConn
	returnAccept <-chan struct{}
}

func (l *transportAdmissionListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	observed := &transportAdmissionConn{Conn: conn, waiting: make(chan struct{})}
	l.accepted <- observed
	if l.returnAccept != nil {
		<-l.returnAccept
	}
	return observed, nil
}

type transportAdmissionConn struct {
	net.Conn
	reads   atomic.Int32
	waiting chan struct{}
	closed  atomic.Bool
}

func (c *transportAdmissionConn) Read(p []byte) (int, error) {
	if c.reads.Add(1) == 2 {
		close(c.waiting)
	}
	return c.Conn.Read(p)
}

func (c *transportAdmissionConn) Close() error {
	err := c.Conn.Close()
	c.closed.Store(true)
	return err
}

func TestMaintenanceTransportAcceptedAdmissionSettlement(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
			for _, phase := range []string{"accept_return", "read_token"} {
				t.Run(fmt.Sprintf("%s/era_%d/%s", mode, protocol, phase), func(t *testing.T) {
					var gate sync.RWMutex
					accepted := make(chan *transportAdmissionConn, 1)
					release := make(chan struct{})
					var releaseOnce sync.Once
					cfg := OwnerConfig{
						ServerID: "transport-admission", IPCPath: shortSocketPath(t), ProtocolEra: protocol,
						TokenHandshake: true, MaintenanceGate: &gate, Logger: log.New(io.Discard, "", 0),
						AdmitMaterialization: func(o *Owner, _ LaunchContext) error {
							listener := &transportAdmissionListener{Listener: o.listener, accepted: accepted}
							if phase == "accept_return" {
								listener.returnAccept = release
							}
							o.listener = listener // construction precedes the accept producer
							return nil
						},
					}
					if mode == "subprocess" {
						cfg.Command, cfg.Args = os.Args[0], []string{"-test.run=^TestMaintenanceAuthorizationHelperProcess$"}
						cfg.Env = map[string]string{"MCPMUX_MAINTENANCE_AUTHORIZATION_HELPER": "1"}
					} else {
						cfg.HandlerFunc = maintenanceAuthorizationStream
					}
					o, err := NewOwner(cfg)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); o.Shutdown() })
					waitForCondition(t, 5*time.Second, func() bool {
						return o.MaterializationState() == MaterializationReady && o.PendingRequests() == 0
					}, "real transport upstream did not become ready")
					launch := o.CurrentLaunchContext()
					if !o.PreRegisterInitial("cafebabe", t.TempDir(), nil) {
						t.Fatal("creating token was not admitted")
					}
					conn, err := ipc.Dial(o.IPCPath())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = conn.Close() })
					var server *transportAdmissionConn
					select {
					case server = <-accepted:
					case <-time.After(time.Second):
						t.Fatal("real IPC connection was not accepted")
					}
					t.Cleanup(func() { _ = server.Close() })
					if phase == "read_token" {
						if _, err := io.WriteString(conn, "c"); err != nil {
							t.Fatal(err)
						}
						select {
						case <-server.waiting:
						case <-time.After(time.Second):
							t.Fatal("readToken did not consume the real prefix before waiting for newline")
						}
					}
					if o.SessionCount() != 0 || o.PendingRequests() != 0 || o.nativeWork.Load() != 0 || server.closed.Load() {
						t.Fatal("pretoken producer fixture unexpectedly settled or registered")
					}
					deadline := time.Now()
					gate.Lock()
					o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: deadline})
					gate.Unlock()
					start := time.Now()
					_, finalized, err := o.FinalizeForRemoval(false, 50*time.Millisecond)
					if finalized || err == nil || o.MaintenanceRetired() {
						t.Fatalf("early completion before accepted admission producer settlement: phase=%s finalized=%t err=%v", phase, finalized, err)
					}
					if time.Since(start) > time.Second || !o.maintenance.Load().DrainDeadline.Equal(deadline) || server.closed.Load() || o.nativeQuiescent() {
						t.Fatal("listener close pretended accepted connection settlement or reset the original deadline")
					}
					select {
					case <-o.Done():
						t.Fatal("OwnerDone closed while the accepted producer was live")
					default:
					}
					if phase == "read_token" {
						if _, err := io.WriteString(conn, "afebabe\n"); err != nil {
							t.Fatal(err)
						}
					} else {
						releaseOnce.Do(func() { close(release) })
					}
					waitForCondition(t, time.Second, server.closed.Load, "real accepted connection did not close after admission resumed")
					waitForCondition(t, time.Second, func() bool {
						_, proven, _ := o.FinalizeForRemoval(false, time.Second)
						return proven
					}, "actual accepted producer return did not permit existing finalization")
					if o.SessionCount() != 0 || len(o.ExportSnapshot().BoundTokens) != 0 || o.protocolEra != protocol || !reflect.DeepEqual(launch, o.CurrentLaunchContext()) || !o.MaintenanceRetired() {
						t.Fatal("late token registered after closure or changed existing launch/era authority")
					}
				})
			}
		}
	}
}

func transportRetireUntil(t *testing.T, o *Owner, deadline time.Time) {
	t.Helper()
	proc := o.currentUpstream()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		t.Fatal("actual EOF exhausted the existing finalization budget")
	}
	_, proven, err := o.FinalizeForRemoval(false, remaining)
	if !proven {
		select {
		case <-o.Done():
			t.Fatal("unproven producer retirement closed OwnerDone")
		default:
		}
		if proc == nil {
			t.Fatalf("healthy no-process transport failed producer join: %v", err)
		}
		// Zero-grace SoftClose can legitimately return before the cooperative
		// handler body returns. Observe actual Done, then use the existing retry.
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-proc.Done:
			timer.Stop()
		case <-timer.C:
			t.Fatalf("actual process producer did not settle within its existing budget: %v", err)
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("actual process return exhausted the original finalization budget")
		}
		_, proven, err = o.FinalizeForRemoval(false, remaining)
	}
	if !proven || !o.MaintenanceRetired() || (proc != nil && !proc.TreesDead()) {
		t.Fatalf("actual native/tree producer retirement remains unproven: proven=%t err=%v", proven, err)
	}
	select {
	case <-o.Done():
	default:
		t.Fatal("proven native/tree retirement did not close OwnerDone")
	}
	if err != nil {
		t.Logf("retirement completed with preserved upstream warning: %v", err)
	}
}

func TestMaintenanceTransportIdleDrainAndShutdownFastPath(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func", "cache_only", "native"} {
		protocols := []era.ProtocolEra{era.EraLegacy, era.EraModern20260728}
		if mode == "cache_only" {
			protocols = []era.ProtocolEra{era.EraLegacy} // R1 snapshot import is intentionally quarantined.
		}
		for _, protocol := range protocols {
			for _, held := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/era_%d/held_%t", mode, protocol, held), func(t *testing.T) {
					var o *Owner
					var gate *sync.RWMutex
					if mode == "subprocess" || mode == "handler_func" {
						o, gate, _ = maintenanceAuthorizationOwner(t, mode, protocol, false, nil)
					} else {
						gate = &sync.RWMutex{}
						cfg := OwnerConfig{IPCPath: shortSocketPath(t), ProtocolEra: protocol, MaintenanceGate: gate, Logger: testLogger(t)}
						var err error
						if mode == "cache_only" {
							o, err = NewOwnerFromSnapshot(cfg, controllerSnapshot())
						} else {
							cfg.SessionHandler = noopSessionHandler{}
							o, err = NewOwner(cfg)
						}
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(o.Shutdown)
					}
					deadline := time.Now().Add(3 * time.Second)
					finalizationDeadline := time.Now().Add(materializationFinalizeTimeout)
					if held {
						gate.Lock()
						o.SetMaintenance(&control.MaintenanceResult{State: control.MaintenanceHolding, DrainDeadline: deadline})
						gate.Unlock()
					}
					if held {
						o.DrainForMaintenance(deadline)
					} else {
						o.DrainRequestsUntil(deadline)
					}
					if !time.Now().Before(deadline) || o.nativeWork.Load() != 0 || o.PendingRequests() != 0 {
						t.Fatal("idle accept producer was mistaken for active grace/drain work")
					}
					transportRetireUntil(t, o, finalizationDeadline)
					if !time.Now().Before(deadline) {
						t.Fatal("healthy idle transport consumed the actual offered drain deadline")
					}
				})
			}
		}
	}
}

func TestMaintenanceTransportNeverStartedAdmissionHasNoPhantomWait(t *testing.T) {
	var captured *Owner
	failure := errors.New("controlled construction refusal")
	start := time.Now()
	_, err := NewOwner(OwnerConfig{
		IPCPath: shortSocketPath(t), HandlerFunc: maintenanceAuthorizationStream, Logger: testLogger(t),
		AdmitMaterialization: func(o *Owner, _ LaunchContext) error { captured = o; return failure },
	})
	if err == nil || captured == nil {
		t.Fatal("constructor did not exercise pre-accept admission failure")
	}
	select {
	case <-captured.Done():
	case <-time.After(time.Second):
		t.Fatal("never-started accept goroutine acquired phantom retirement authority")
	}
	if time.Since(start) > time.Second {
		t.Fatal("constructor failure waited for a goroutine that was never launched")
	}
	// Admission closure prevents a subsequently offered real session as well.
	reader, writer := io.Pipe()
	defer writer.Close()
	s := NewSession(reader, &safeBuf{})
	captured.AddSession(s)
	if !s.IsClosed() || captured.SessionCount() != 0 {
		t.Fatal("failed construction admitted a session after listener closure")
	}
}

func TestMaintenanceTransportPlainReaderRealEOFFastPath(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []era.ProtocolEra{era.EraLegacy, era.EraModern20260728} {
			t.Run(fmt.Sprintf("%s/era_%d", mode, protocol), func(t *testing.T) {
				o, _, _ := maintenanceAuthorizationOwner(t, mode, protocol, false, nil)
				reader, writer := io.Pipe()
				s := NewSession(reader, &safeBuf{})
				o.AddSession(s)
				// The existing finalization budget starts at the real EOF effect,
				// not at a later assertion or a race-runtime wall-clock threshold.
				deadline := time.Now().Add(materializationFinalizeTimeout)
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				waitForCondition(t, time.Second, func() bool { return o.SessionCount() == 0 && o.nativeWork.Load() == 0 }, "ordinary real EOF did not settle the plain reader")
				transportRetireUntil(t, o, deadline)
				if !time.Now().Before(deadline) {
					t.Fatal("ordinary no-hook EOF exhausted its original finalization budget")
				}
			})
		}
	}
}
