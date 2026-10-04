package owner_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// The same real pipe-reader exit seam used by owner transport tests. Closing the
// pipe is real transport retirement; returning from Read remains controlled.
type transportHoldReader struct {
	io.ReadCloser
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *transportHoldReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil {
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
	return n, err
}

func transportHoldStream(_ context.Context, stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return err
		}
		if len(request.ID) == 0 {
			continue
		}
		result := `{"tools":[]}`
		if request.Method == "initialize" {
			result = `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"transport-hold","version":"1"}}`
		}
		if _, err := fmt.Fprintf(stdout, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", request.ID, result); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func transportHoldWait(t *testing.T, condition func() bool, reason string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(reason)
}

func TestMaintenanceTransportPublicHoldRetainsPlainReader(t *testing.T) {
	for _, mode := range []string{"subprocess", "handler_func"} {
		for _, protocol := range []string{"", "2026-07-28"} {
			for _, ordinaryFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/era_%s/ordinary_first_%t", mode, protocol, ordinaryFirst), func(t *testing.T) {
					config := t.TempDir()
					for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
						t.Setenv(key, config)
					}
					file, err := os.CreateTemp("", "m-tr-*.sock")
					if err != nil {
						t.Fatal(err)
					}
					path := file.Name()
					_ = file.Close()
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { ipc.Cleanup(path) })
					cfg := daemon.Config{ControlPath: path, Namespace: filepath.Base(path), SkipSnapshot: true, Logger: log.New(io.Discard, "", 0)}
					if mode == "handler_func" {
						cfg.HandlerFunc = transportHoldStream
					}
					d, err := daemon.New(cfg)
					if err != nil {
						t.Fatal(err)
					}
					release := make(chan struct{})
					var releaseOnce sync.Once
					var held *control.MaintenanceResult
					t.Cleanup(func() {
						releaseOnce.Do(func() { close(release) })
						if held != nil {
							deadline := time.Now().Add(5 * time.Second)
							for time.Now().Before(deadline) {
								if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: held.HoldID}); err == nil {
									break
								}
								time.Sleep(5 * time.Millisecond)
							}
						}
						d.Shutdown()
					})
					request := control.Request{
						Command: os.Args[0], Args: []string{"-test.run=^TestMaintenanceAuthorizationHelperProcess$"}, Mode: "isolated", Cwd: config,
						Env: map[string]string{"MCPMUX_MAINTENANCE_AUTHORIZATION_HELPER": "1", "TRANSPORT_GENERATION": "original"}, ProtocolEra: protocol,
					}
					_, sid, _, err := d.Spawn(request)
					if err != nil {
						t.Fatal(err)
					}
					entry := d.Entry(sid)
					if entry == nil || entry.Owner == nil || entry.OwnerGeneration == "" {
						t.Fatal("real consumer admission did not retain exact generation authority")
					}
					generation := entry.OwnerGeneration
					transportHoldWait(t, func() bool {
						return entry.Owner.MaterializationState() == owner.MaterializationReady && entry.Owner.PendingRequests() == 0
					}, "real subprocess/HandlerFunc generation did not become ready")
					reader, writer := io.Pipe()
					t.Cleanup(func() { _ = writer.Close() })
					exit := &transportHoldReader{ReadCloser: reader, entered: make(chan struct{}), release: release}
					session := owner.NewSession(exit, io.Discard)
					session.Cwd, session.Env = config, request.Env
					entry.Owner.AddSession(session)
					stopped := make(chan error, 1)
					if ordinaryFirst {
						go func() { _, err := d.HandleStopOwner(control.Request{ServerID: sid}); stopped <- err }()
						select {
						case <-exit.entered:
						case <-time.After(5 * time.Second):
							t.Fatal("ordinary removal did not reach the real closed pipe Read barrier")
						}
						select {
						case err := <-stopped:
							t.Fatalf("early owner forget before plain reader deferred settlement during ordinary removal: %v", err)
						default:
						}
						if d.Entry(sid) != entry || entry.OwnerGeneration != generation || entry.Owner.SessionCount() != 1 {
							t.Fatal("ordinary removal erased the exact live reader identity before a later hold")
						}
					}
					type holdResponse struct {
						result *control.MaintenanceResult
						err    error
					}
					completed := make(chan holdResponse, 1)
					ttl := int64(60000)
					go func() {
						result, err := control.SendMaintenance(path, control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: &ttl}, 10*time.Second)
						completed <- holdResponse{result, err}
					}()
					// HOLDING status proves actual request admission; the ledger below
					// separately proves durable state and the original finite clocks.
					transportHoldWait(t, func() bool {
						select {
						case response := <-completed:
							t.Fatalf("early public completion before plain reader deferred settlement: result=%+v err=%v", response.result, response.err)
						default:
						}
						results := d.HandleStatus()["maintenance"].([]control.MaintenanceResult)
						if len(results) == 0 {
							return false
						}
						if len(results) != 1 || results[0].State != control.MaintenanceHolding || results[0].TreesRetired || results[0].ServerID != sid {
							t.Fatalf("early HELD before plain reader deferred settlement: %+v", results)
						}
						copy := results[0]
						held = &copy
						return true
					}, "public hold did not admit and durably publish its exact HOLDING lease")
					select {
					case <-exit.entered:
					case <-time.After(5 * time.Second):
						t.Fatal("real connection close never reached the controlled pipe Read return")
					}
					if !session.IsClosed() || d.Entry(sid) != entry || entry.OwnerGeneration != generation || entry.Owner.SessionCount() != 1 || entry.Owner.PendingRequests() != 0 || entry.Owner.MaintenanceRetired() {
						t.Fatal("blocked hold erased plain producer, context generation, or request-only metrics")
					}
					select {
					case <-entry.Owner.Done():
						t.Fatal("OwnerDone closed while real plain Read remained live")
					default:
					}
					deadline, expiry, holdID := held.DrainDeadline, held.ExpiresAt, held.HoldID
					parent, err := filepath.EvalSymlinks(filepath.Dir(path))
					if err != nil {
						t.Fatal(err)
					}
					ledgers, err := filepath.Glob(filepath.Join(serverid.DaemonLockPath(parent, cfg.Namespace)+".maintenance", "*", "ledger.json"))
					if err != nil || len(ledgers) != 1 {
						t.Fatalf("exact public hold lacks one durable ledger: paths=%v err=%v", ledgers, err)
					}
					assertDurable := func(state control.MaintenanceState) []byte {
						t.Helper()
						data, err := os.ReadFile(ledgers[0])
						if err != nil {
							t.Fatal(err)
						}
						var ledger struct {
							Leases []control.MaintenanceResult `json:"leases"`
						}
						if err := json.Unmarshal(data, &ledger); err != nil {
							t.Fatal(err)
						}
						if state == control.MaintenanceReleased {
							if len(ledger.Leases) != 0 {
								t.Fatal("successful resume did not durably clear its retired lease")
							}
							return data
						}
						if len(ledger.Leases) != 1 || ledger.Leases[0].HoldID != holdID || ledger.Leases[0].State != state || !ledger.Leases[0].DrainDeadline.Equal(deadline) || !ledger.Leases[0].ExpiresAt.Equal(expiry) {
							t.Fatalf("durable public lease lost exact state or original clocks: %+v", ledger.Leases)
						}
						return data
					}
					holdingBytes := assertDurable(control.MaintenanceHolding)
					// acquireMaintenance retains this nonblocking namespace lock until
					// the actual reader settles. Overlapping resume cannot reach its
					// lease-state check; prove contention rather than a storage failure.
					lock, lockErr := ipc.AcquireFileLock(serverid.DaemonLockPath(parent, cfg.Namespace))
					if lock != nil {
						_ = lock.Close()
					}
					if !errors.Is(lockErr, ipc.ErrFileLocked) {
						t.Fatalf("admitted public hold did not retain its exact namespace lock: %v", lockErr)
					}
					if _, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: holdID}); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
						t.Fatalf("overlapping resume did not refuse the proven namespace contention: %v", err)
					}
					results := d.HandleStatus()["maintenance"].([]control.MaintenanceResult)
					if len(results) != 1 || results[0].HoldID != holdID || results[0].State != control.MaintenanceHolding || results[0].TreesRetired || !results[0].DrainDeadline.Equal(deadline) || !results[0].ExpiresAt.Equal(expiry) {
						t.Fatal("blocked consumer changed the original finite lease timing")
					}
					if !bytes.Equal(holdingBytes, assertDurable(control.MaintenanceHolding)) {
						t.Fatal("refused overlapping resume changed the committed HOLDING ledger bytes")
					}
					select {
					case response := <-completed:
						t.Fatalf("early public completion/HELD while actual Read remains blocked: result=%+v err=%v", response.result, response.err)
					default:
					}
					if ordinaryFirst {
						select {
						case err := <-stopped:
							t.Fatalf("ordinary removal completed before the real reader returned: %v", err)
						default:
						}
					}
					releaseOnce.Do(func() { close(release) })
					select {
					case response := <-completed:
						if response.err != nil || response.result == nil || response.result.State != control.MaintenanceHeld || !response.result.TreesRetired || response.result.HoldID != holdID || !response.result.DrainDeadline.Equal(deadline) || !response.result.ExpiresAt.Equal(expiry) {
							t.Fatalf("actual Read return did not complete the original public hold: result=%+v err=%v", response.result, response.err)
						}
						held = response.result
					case <-time.After(10 * time.Second):
						t.Fatal("admitted public hold did not finish after actual producer release")
					}
					if ordinaryFirst {
						select {
						case err := <-stopped:
							if err != nil {
								t.Fatalf("admitted ordinary removal failed after real reader settlement: %v", err)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("ordinary removal did not finish after actual producer release")
						}
					}
					transportHoldWait(t, func() bool {
						results := d.HandleStatus()["maintenance"].([]control.MaintenanceResult)
						return len(results) == 1 && results[0].State == control.MaintenanceHeld && results[0].TreesRetired && results[0].HoldID == holdID && results[0].DrainDeadline.Equal(deadline) && results[0].ExpiresAt.Equal(expiry) && d.Entry(sid) == nil && entry.Owner.MaintenanceRetired()
					}, "real Read return did not produce HELD through existing exact-entry finalization")
					assertDurable(control.MaintenanceHeld)
					if protocol != "" && entry.ProtocolEra != era.EraModern20260728 {
						t.Fatal("reader settlement downgraded modern owner authority")
					}
					select {
					case <-entry.Owner.Done():
					default:
						t.Fatal("HELD preceded actual owner producer completion")
					}
					if _, err := control.SendMaintenance(path, control.Request{Cmd: "resume", HoldID: holdID}, 5*time.Second); err != nil {
						t.Fatal(err)
					}
					assertDurable(control.MaintenanceReleased)
					held = nil
				})
			}
		}
	}
}
