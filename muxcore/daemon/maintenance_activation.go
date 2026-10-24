package daemon

import (
	"errors"
	"io"
	"strings"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

// CheckMaintenanceForActivation checks the same durable authority loaded by New.
// The caller must already hold serverid.DaemonLockPath for this namespace across
// this check and activation. It never locks, writes, removes or expires authority.
// Expired HELD remains fenced until the aware daemon durably releases it. A
// failed acquisition without durable commitment is not an installer grant; live
// aware status additionally protects its in-memory failclosed state.
func CheckMaintenanceForActivation(namespace, controlEndpoint string) error {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		namespace = "mcp-mux"
	}
	if controlEndpoint == "" {
		controlEndpoint = serverid.DaemonControlPath("", namespace)
	}
	_, _, ledger, err := readMaintenanceAuthority(namespace, controlEndpoint)
	if err != nil {
		return err
	}
	return maintenanceActivationError(ledger)
}

// AcquireMaintenanceForActivation holds the original existing namespace lock and
// native path guards until Close. It never creates the lock or changes authority.
// Callers must also check live status and retain the closer through publication.
func AcquireMaintenanceForActivation(namespace, controlEndpoint string) (io.Closer, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		namespace = "mcp-mux"
	}
	if controlEndpoint == "" {
		controlEndpoint = serverid.DaemonControlPath("", namespace)
	}
	guard, canonical, err := openMaintenanceAuthority(namespace, controlEndpoint)
	if err != nil {
		return nil, err
	}
	file, err := guard.openNamespaceLock()
	if err != nil {
		_ = guard.Close()
		return nil, err
	}
	lock, err := ipc.AcquireFileLockFromFile(file)
	if err != nil {
		_ = guard.Close()
		return nil, err
	}
	activation := &maintenanceActivationGuard{path: guard, lock: lock}
	_, _, ledger, err := readMaintenanceAuthorityGuarded(namespace, canonical, guard)
	if err == nil {
		err = maintenanceActivationError(ledger)
	}
	if err != nil {
		_ = activation.Close()
		return nil, err
	}
	return activation, nil
}

type maintenanceActivationGuard struct {
	path *maintenancePathGuard
	lock io.Closer
}

func (g *maintenanceActivationGuard) Close() error {
	return errors.Join(g.lock.Close(), g.path.Close())
}

func maintenanceActivationError(ledger *maintenanceLedger) error {
	if ledger == nil || len(ledger.Leases) == 0 {
		return nil
	}
	record := ledger.Leases[0]
	result := control.MaintenanceResult{HoldID: record.HoldID, State: record.State, ExpiresAt: record.ExpiresAt, DrainDeadline: record.DrainDeadline, TreesRetired: record.State == control.MaintenanceHeld}
	code := control.ErrMaintenanceHeld.Code
	if result.State != control.MaintenanceHeld {
		result.State = control.MaintenanceRetirementBlocked
		code = control.ErrMaintenanceRetirementBlocked.Code
	}
	return &control.MaintenanceError{Code: code, Result: &result}
}
