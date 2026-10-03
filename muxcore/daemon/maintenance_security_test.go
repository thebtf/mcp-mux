package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func maintenanceSecurityHeld(t *testing.T) (*Daemon, control.Request, control.MaintenanceResult) {
	t.Helper()
	// UserConfigDir uses HOME rather than XDG_CONFIG_HOME on Darwin.
	t.Setenv("HOME", t.TempDir())
	d := maintenanceDaemon(t)
	req := control.Request{Command: "maintenance-security-cache-only", Mode: "global", Cwd: t.TempDir()}
	d.updateTemplate(req.Command, req.Args, daemonMaterializationSnapshot(false))
	_, sid, _, err := d.Spawn(req)
	if err != nil {
		t.Fatalf("fixture admission: %v", err)
	}
	result, err := d.HandleMaintenance(control.Request{Cmd: "hold", ServerID: sid, HoldTTLMS: maintenanceTTL(600000)})
	if err != nil || result.State != control.MaintenanceHeld || !result.TreesRetired || !time.Now().Before(result.ExpiresAt) {
		t.Fatalf("acquire unexpired retired fixture lease: %+v %v", result, err)
	}
	if _, _, _, err := d.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("fixture matching demand was not fenced: %v", err)
	}
	return d, req, result
}

func maintenanceSecurityRejectAuthority(t *testing.T, d *Daemon, data []byte) {
	t.Helper()
	if !json.Valid(data) {
		t.Fatal("fixture must be valid JSON, not a syntax-error rejection")
	}
	endpoint, namespace, path := d.ctlSrv.SocketPath(), d.namespace, d.maintenancePath
	d.shutdown(nil)
	if err := writeMaintenanceLedger(path, data); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readMaintenanceAuthority(namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Errorf("malformed authority accepted by store: %v", err)
	}
	recovered, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
	if recovered != nil {
		t.Cleanup(func() { recovered.shutdown(nil) })
	}
	if !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Errorf("malformed authority did not refuse aware startup: %v", err)
	}
	if ipc.IsAvailable(endpoint) {
		t.Error("malformed authority bound a control admission listener")
	}
	if err := CheckMaintenanceForActivation(namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Errorf("malformed authority did not refuse activation: %v", err)
	}
}

func TestMaintenanceSecurity001EndpointAliasKeepsRecoveryFence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix endpoint-directory symlink semantics; Windows symlink privileges and named-pipe endpoints are different")
	}
	config := t.TempDir()
	t.Setenv("HOME", config)
	t.Setenv("APPDATA", config)
	t.Setenv("XDG_CONFIG_HOME", config)
	namespace := "maintenance-security-alias"
	req, _, _, _ := maintenanceHelperRequest(t)
	now := time.Now().UTC()
	result := control.MaintenanceResult{HoldID: "alias-fixture", State: control.MaintenanceHeld, ExpiresAt: now.Add(10 * time.Minute), DrainDeadline: now, TreesRetired: true}
	base := t.TempDir()
	realDir, aliasDir := filepath.Join(base, "real"), filepath.Join(base, "alias")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Fatalf("create Unix endpoint-directory alias: %v", err)
	}
	endpoint := filepath.Join(aliasDir, "control.sock")
	beforeCanonical := canonicalMaintenancePath(endpoint)
	beforePath, beforeScope, _, err := readMaintenanceAuthority(namespace, endpoint)
	if err != nil {
		t.Fatal(err)
	}

	// Persist an unexpired HELD store fixture without creating an IPC listener
	// or asserting process retirement. The real resolver and recovery admission
	// gate remain exercisable even on filesystems that cannot bind Unix sockets.
	before := &Daemon{namespace: namespace}
	if err := before.loadMaintenance(endpoint); err != nil {
		t.Fatal(err)
	}
	key := before.maintenanceContext(era.EraLegacy, req.Command, req.Args, req.Cwd, mergeEnv(req.Env))
	lease := &maintenanceLease{record: maintenanceRecord{HoldID: result.HoldID, Keys: []string{key}}, result: result}
	if err := before.persistMaintenanceLocked(map[string]*maintenanceLease{result.HoldID: lease}); err != nil {
		t.Fatal(err)
	}
	if err := before.loadMaintenance(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := before.maintenanceAdmission(key); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Fatalf("absent-leaf fixture did not recover matching fence: %v", err)
	}
	// A harmless file exercises full-path symlink resolution just like a stale
	// socket does, without starting or stopping another daemon.
	if err := os.WriteFile(filepath.Join(realDir, "control.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	afterCanonical := canonicalMaintenancePath(endpoint)
	afterPath, afterScope, _, err := readMaintenanceAuthority(namespace, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if beforeCanonical != afterCanonical {
		t.Errorf("endpoint leaf creation changed canonical endpoint: before %s; after %s", beforeCanonical, afterCanonical)
	}
	if beforePath != afterPath || beforeScope != afterScope {
		t.Errorf("endpoint leaf creation changed authority identity: before %s %s; after %s %s", beforePath, beforeScope, afterPath, afterScope)
	}
	recovered := &Daemon{namespace: namespace}
	if err := recovered.loadMaintenance(endpoint); err != nil {
		t.Fatal(err)
	}
	matchingKey := recovered.maintenanceContext(era.EraLegacy, req.Command, req.Args, req.Cwd, mergeEnv(req.Env))
	if err := recovered.maintenanceAdmission(matchingKey); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Errorf("present-leaf recovery lost matching admission fence: %v", err)
	}
	if err := CheckMaintenanceForActivation(namespace, endpoint); !errors.Is(err, control.ErrMaintenanceHeld) {
		t.Errorf("present-leaf recovery lost persisted activation fence: %v", err)
	}
}

func TestMaintenanceSecurity002NoncanonicalKeyRefusesStartup(t *testing.T) {
	d, _, _ := maintenanceSecurityHeld(t)
	data, err := os.ReadFile(d.maintenancePath)
	if err != nil {
		t.Fatal(err)
	}
	var ledger maintenanceLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	key := ledger.Leases[0].Keys[0]
	upper := key[:3] + strings.ToUpper(key[3:])
	if upper == key {
		t.Fatal("fixture digest has no alphabetic hex digits to change")
	}
	data = bytes.Replace(data, []byte(fmt.Sprintf("%q", key)), []byte(fmt.Sprintf("%q", upper)), 1)
	maintenanceSecurityRejectAuthority(t, d, data)
}

func TestMaintenanceSecurity003DuplicateMembersRefuseStartup(t *testing.T) {
	for _, test := range []struct {
		name   string
		member string
		nested bool
	}{
		{name: "leases", member: "leases"},
		{name: "leases_case_variant", member: "Leases"},
		{name: "state", member: "state", nested: true},
		{name: "state_case_variant", member: "State", nested: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, _, result := maintenanceSecurityHeld(t)
			data, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			if test.nested {
				// Last-member-wins decoding must not manufacture HELD/tree-death
				// authority from an ambiguous HOLDING record.
				original := fmt.Sprintf(`"state":%q`, result.State)
				replacement := fmt.Sprintf(`"state":%q,%q:%q`, control.MaintenanceHolding, test.member, control.MaintenanceHeld)
				data = bytes.Replace(data, []byte(original), []byte(replacement), 1)
			} else {
				data = []byte(strings.TrimSuffix(string(data), "}") + fmt.Sprintf(`,%q:[]}`, test.member))
			}
			maintenanceSecurityRejectAuthority(t, d, data)
		})
	}
}

func TestMaintenanceSecurity004FailedReleaseAfterPublicationKeepsRecoveryFence(t *testing.T) {
	d, req, result := maintenanceSecurityHeld(t)
	publishedRelease := false
	d.maintenanceGate.Lock()
	d.maintenanceCommit = func(data []byte) error {
		var ledger maintenanceLedger
		if err := json.Unmarshal(data, &ledger); err != nil {
			return err
		}
		if err := writeMaintenanceLedger(d.maintenancePath, data); err != nil {
			return err
		}
		if len(ledger.Leases) == 0 {
			publishedRelease = true
			// Model Unix directory open/fsync failure after a successful rename.
			// This fault boundary is portable; no actual OS fsync fault is claimed.
			return errors.New("injected post-publication durability failure")
		}
		return nil
	}
	d.maintenanceGate.Unlock()
	_, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID})
	if !publishedRelease || !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("release did not reach published-but-failed persistence boundary: published=%t err=%v", publishedRelease, err)
	}
	if _, _, _, err := d.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) && !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("failed release opened live matching admission: %v", err)
	}
	endpoint, namespace := d.ctlSrv.SocketPath(), d.namespace
	// As in existing recovery tests, bypass controlled-shutdown admission only
	// to discard volatile state. Retired fixture owners leave no process tree.
	d.shutdown(nil)
	recovered, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
	if errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		if ipc.IsAvailable(endpoint) {
			t.Fatal("failed recovery bound control admission")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovered.shutdown(nil) })
	// A cache-only template makes a missed fence observable as successful
	// admission, without trying to execute an arbitrary command on the host.
	recovered.updateTemplate(req.Command, req.Args, daemonMaterializationSnapshot(false))
	if _, _, _, err := recovered.Spawn(req); !errors.Is(err, control.ErrMaintenanceHeld) && !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
		t.Fatalf("published-but-failed release left recovery admission fenceless: %v", err)
	}
}

func TestMaintenanceSecurity004TransactionBoundaries(t *testing.T) {
	for _, operation := range []string{"release", "initial_hold", "clocked_hold"} {
		t.Run(operation, func(t *testing.T) {
			for _, test := range []struct {
				phase string
				after bool
			}{
				{phase: "prepare"},
				{phase: "prepare", after: true},
				{phase: "publish"},
				{phase: "publish", after: true},
				{phase: "finalize"},
				{phase: "finalize", after: true},
			} {
				t.Run(fmt.Sprintf("%s_after_%t", test.phase, test.after), func(t *testing.T) {
					d, req, result := maintenanceSecurityHeld(t)
					endpoint, namespace := d.ctlSrv.SocketPath(), d.namespace
					data, err := os.ReadFile(d.maintenancePath)
					if err != nil {
						t.Fatal(err)
					}
					var previous maintenanceLedger
					if err := json.Unmarshal(data, &previous); err != nil {
						t.Fatal(err)
					}
					d.shutdown(nil)
					holding := previous.Leases[0]
					holding.State = control.MaintenanceHolding
					holding.ExpiresAt = time.Now().UTC().Add(-time.Minute)
					holding.DrainDeadline = holding.ExpiresAt.Add(-time.Minute)
					if operation != "release" {
						predecessor := previous.Leases
						previous.Transaction = maintenanceDigest("security-predecessor", t.Name())
						previous.Leases = []maintenanceRecord{}
						if operation == "clocked_hold" {
							previous.Leases = []maintenanceRecord{holding}
						}
						if err := commitMaintenanceStore(d.maintenancePath, previous, predecessor, writeMaintenanceLedger); err != nil {
							t.Fatal(err)
						}
					}
					next := maintenanceLedger{Version: maintenanceSchema, Scope: previous.Scope, Transaction: maintenanceDigest("security-transaction", t.Name()), Leases: []maintenanceRecord{}}
					if operation != "release" {
						if operation == "clocked_hold" {
							holding.ExpiresAt = holding.ExpiresAt.Add(time.Second)
							holding.DrainDeadline = holding.DrainDeadline.Add(time.Second)
						}
						next.Leases = []maintenanceRecord{holding}
					}
					faulted := false
					err = commitMaintenanceStore(d.maintenancePath, next, previous.Leases, func(path string, data []byte) error {
						phase := "publish"
						if path == maintenanceTransactionPath(d.maintenancePath) {
							var transaction maintenanceTransaction
							if err := json.Unmarshal(data, &transaction); err != nil {
								return err
							}
							phase = "prepare"
							if transaction.State == "committed" {
								phase = "finalize"
							}
						}
						if phase == test.phase && !test.after {
							faulted = true
							return errors.New("injected before publication")
						}
						if err := writeMaintenanceLedger(path, data); err != nil {
							return err
						}
						if phase == test.phase {
							faulted = true
							return errors.New("injected after publication")
						}
						return nil
					})
					if !faulted || !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
						t.Fatalf("fault boundary was not returned: reached=%t err=%v", faulted, err)
					}
					// A published FINALIZE certificate proves the earlier acknowledged
					// target commit, never API success or tree death for HOLDING.
					completed := test.phase == "finalize" && test.after
					unchanged := test.phase == "prepare" && !test.after
					valid := completed || unchanged
					want := previous.Leases
					if completed {
						want = next.Leases
					}
					_, _, authority, readErr := readMaintenanceAuthority(namespace, endpoint)
					if valid {
						if readErr != nil || authority == nil || len(authority.Leases) != len(want) {
							t.Fatalf("lost acknowledged authority: %+v %v", authority, readErr)
						}
						if len(want) == 1 && (authority.Leases[0].State != want[0].State || !authority.Leases[0].ExpiresAt.Equal(want[0].ExpiresAt) || !authority.Leases[0].DrainDeadline.Equal(want[0].DrainDeadline)) {
							t.Fatalf("recovery changed acknowledged fence timing/state: %+v want %+v", authority.Leases[0], want[0])
						}
					} else if !errors.Is(readErr, control.ErrMaintenancePersistenceFailed) {
						t.Fatalf("pending transaction became usable authority: %+v %v", authority, readErr)
					}
					activationErr := CheckMaintenanceForActivation(namespace, endpoint)
					if !valid {
						if !errors.Is(activationErr, control.ErrMaintenancePersistenceFailed) {
							t.Fatalf("pending authority permitted activation: %v", activationErr)
						}
					} else if len(want) == 0 {
						if activationErr != nil {
							t.Fatalf("acknowledged empty authority retained a fence: %v", activationErr)
						}
					} else {
						code := control.ErrMaintenanceHeld
						if want[0].State != control.MaintenanceHeld {
							code = control.ErrMaintenanceRetirementBlocked
						}
						if !errors.Is(activationErr, code) {
							t.Fatalf("acknowledged fence permitted activation: %v", activationErr)
						}
					}
					recovered, startupErr := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
					if recovered != nil {
						t.Cleanup(func() { recovered.shutdown(nil) })
					}
					if !valid {
						if !errors.Is(startupErr, control.ErrMaintenancePersistenceFailed) || ipc.IsAvailable(endpoint) {
							t.Fatalf("pending recovery opened control admission: err=%v available=%t", startupErr, ipc.IsAvailable(endpoint))
						}
						return
					}
					if startupErr != nil {
						t.Fatal(startupErr)
					}
					recovered.updateTemplate(req.Command, req.Args, daemonMaterializationSnapshot(false))
					_, _, _, admissionErr := recovered.Spawn(req)
					if len(want) == 0 && admissionErr != nil || len(want) > 0 && !errors.Is(admissionErr, control.ErrMaintenanceHeld) {
						t.Fatalf("recovery admission did not apply transaction outcome: leases=%d err=%v", len(want), admissionErr)
					}
					if len(want) > 0 && want[0].State == control.MaintenanceHolding {
						recovered.reconcileMaintenance()
						states := recovered.maintenanceResults()
						if len(states) != 1 || states[0].State != control.MaintenanceRetirementBlocked || states[0].TreesRetired || !states[0].ExpiresAt.Equal(want[0].ExpiresAt) || !states[0].DrainDeadline.Equal(want[0].DrainDeadline) {
							t.Fatalf("incomplete clock recovery invented tree death or reset time: %+v", states)
						}
						if _, err := control.SendMaintenance(endpoint, control.Request{Cmd: "resume", HoldID: result.HoldID}, time.Second); !errors.Is(err, control.ErrMaintenanceRetirementBlocked) {
							t.Fatalf("incomplete clock recovery permitted resume: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestMaintenanceSecurity004AcknowledgedReleaseDoesNotResurrectPredecessor(t *testing.T) {
	d, req, result := maintenanceSecurityHeld(t)
	endpoint, namespace := d.ctlSrv.SocketPath(), d.namespace
	if released, err := d.HandleMaintenance(control.Request{Cmd: "resume", HoldID: result.HoldID}); err != nil || released.State != control.MaintenanceReleased {
		t.Fatalf("acknowledged release: %+v %v", released, err)
	}
	d.shutdown(nil)
	recovered, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovered.shutdown(nil) })
	if err := CheckMaintenanceForActivation(namespace, endpoint); err != nil {
		t.Fatalf("completed release retained activation fence: %v", err)
	}
	recovered.updateTemplate(req.Command, req.Args, daemonMaterializationSnapshot(false))
	if _, _, _, err := recovered.Spawn(req); err != nil {
		t.Fatalf("completed release resurrected matching predecessor: %v", err)
	}
}

func TestMaintenanceSecurity004IncompleteOrAmbiguousAggregateRefusesStartup(t *testing.T) {
	for _, fault := range []string{"missing_ledger", "missing_transaction", "both_missing", "unknown_member", "unknown_transaction_schema", "duplicate_transaction_state", "duplicate_transaction_state_case_variant"} {
		t.Run(fault, func(t *testing.T) {
			d, _, _ := maintenanceSecurityHeld(t)
			endpoint, namespace := d.ctlSrv.SocketPath(), d.namespace
			path, transactionPath := d.maintenancePath, maintenanceTransactionPath(d.maintenancePath)
			d.shutdown(nil)
			switch fault {
			case "missing_ledger", "both_missing":
				if err := os.Rename(path, filepath.Join(filepath.Dir(path), ".ledger-abandoned-main")); err != nil {
					t.Fatal(err)
				}
			}
			switch fault {
			case "missing_transaction", "both_missing":
				if err := os.Rename(transactionPath, filepath.Join(filepath.Dir(path), ".ledger-abandoned-transaction")); err != nil {
					t.Fatal(err)
				}
			case "unknown_member":
				if err := writeMaintenanceLedger(filepath.Join(filepath.Dir(path), "unknown.json"), []byte(`{}`)); err != nil {
					t.Fatal(err)
				}
			case "unknown_transaction_schema":
				if err := writeMaintenanceLedger(transactionPath, []byte(`{"version":999}`)); err != nil {
					t.Fatal(err)
				}
			case "duplicate_transaction_state", "duplicate_transaction_state_case_variant":
				data, err := os.ReadFile(transactionPath)
				if err != nil {
					t.Fatal(err)
				}
				member := "state"
				if fault == "duplicate_transaction_state_case_variant" {
					member = "State"
				}
				data = bytes.Replace(data, []byte(`"state":"committed"`), []byte(fmt.Sprintf(`"state":"prepared",%q:"committed"`, member)), 1)
				if !json.Valid(data) {
					t.Fatal("transaction fixture is not valid JSON")
				}
				if err := writeMaintenanceLedger(transactionPath, data); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, _, err := readMaintenanceAuthority(namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
				t.Fatalf("incomplete/ambiguous aggregate read accepted: %v", err)
			}
			recovered, err := New(Config{ControlPath: endpoint, Namespace: namespace, SkipSnapshot: true, Logger: testLogger(t)})
			if recovered != nil {
				t.Cleanup(func() { recovered.shutdown(nil) })
			}
			if !errors.Is(err, control.ErrMaintenancePersistenceFailed) || ipc.IsAvailable(endpoint) {
				t.Fatalf("invalid aggregate opened startup admission: err=%v available=%t", err, ipc.IsAvailable(endpoint))
			}
			if err := CheckMaintenanceForActivation(namespace, endpoint); !errors.Is(err, control.ErrMaintenancePersistenceFailed) {
				t.Fatalf("invalid aggregate opened activation: %v", err)
			}
		})
	}
}

func TestMaintenanceSecurity002MalformedKeysRefuseEvenMatchingCommitCertificate(t *testing.T) {
	for _, fault := range []string{"noncanonical", "duplicate_key", "overlapping_key", "duplicate_hold_id"} {
		t.Run(fault, func(t *testing.T) {
			d, _, _ := maintenanceSecurityHeld(t)
			data, err := os.ReadFile(d.maintenancePath)
			if err != nil {
				t.Fatal(err)
			}
			var ledger maintenanceLedger
			if err := json.Unmarshal(data, &ledger); err != nil {
				t.Fatal(err)
			}
			transactionData, err := os.ReadFile(maintenanceTransactionPath(d.maintenancePath))
			if err != nil {
				t.Fatal(err)
			}
			var transaction maintenanceTransaction
			if err := json.Unmarshal(transactionData, &transaction); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "noncanonical":
				ledger.Leases[0].Keys[0] = "v1:" + strings.Repeat("A", 64)
			case "duplicate_key":
				ledger.Leases[0].Keys = append(ledger.Leases[0].Keys, ledger.Leases[0].Keys[0])
			case "overlapping_key", "duplicate_hold_id":
				second := ledger.Leases[0]
				if fault == "overlapping_key" {
					second.HoldID += "-other"
				} else {
					second.Keys = []string{maintenanceDigest("distinct-security-context")}
				}
				ledger.Leases = append(ledger.Leases, second)
			}
			data, err = json.Marshal(ledger)
			if err != nil {
				t.Fatal(err)
			}
			// The certificate is an acknowledgment/pairing check, not a signature
			// or a substitute for validating malformed private context metadata.
			transaction.Target = maintenanceLedgerDigest(data)
			transactionData, err = json.Marshal(transaction)
			if err != nil {
				t.Fatal(err)
			}
			d.shutdown(nil)
			if err := writeMaintenanceLedger(maintenanceTransactionPath(d.maintenancePath), transactionData); err != nil {
				t.Fatal(err)
			}
			maintenanceSecurityRejectAuthority(t, d, data)
		})
	}
}
