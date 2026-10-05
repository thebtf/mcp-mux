package daemon

import (
	"strings"

	"github.com/thebtf/mcp-mux/muxcore/control"
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
