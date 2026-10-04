package engine

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
)

func isMaintenanceError(err error) bool {
	var refusal *control.MaintenanceError
	return errors.As(err, &refusal)
}

func isMaintenanceFence(err error) bool {
	return errors.Is(err, control.ErrMaintenanceHeld) || errors.Is(err, control.ErrMaintenanceRetirementBlocked) || errors.Is(err, control.ErrMaintenancePersistenceFailed)
}

func activationEndpointAbsent(err error) bool {
	var socketErr *net.OpError
	if errors.As(err, &socketErr) {
		return socketErr.Op == "dial" && (errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOTSOCK))
	}
	// Named-pipe dialing reports a missing pipe as an open PathError.
	var pipeErr *os.PathError
	return errors.As(err, &pipeErr) && pipeErr.Op == "open" && errors.Is(err, os.ErrNotExist)
}

// The caller owns the namespace lock until activation and replacement settle.
func (e *MuxEngine) checkMaintenanceForActivation() error {
	path := e.ControlSocketPath()
	if err := daemon.CheckMaintenanceForActivation(e.cfg.Namespace, path); err != nil {
		return err
	}
	response, err := engineControlSend(path, control.Request{Cmd: "status"})
	if err != nil {
		if activationEndpointAbsent(err) {
			return nil // Persisted authority was proven clear under the same lock.
		}
		return err
	}
	if err := response.Err(); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(response.Data, &fields) != nil || fields == nil {
		return control.ErrMaintenanceInvalid
	}
	if raw, present := fields["maintenance_error_code"]; present {
		var code control.MaintenanceErrorCode
		if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &code) != nil {
			return control.ErrMaintenanceInvalid
		}
		if code != "" {
			return (&control.Response{ErrorCode: code}).Err()
		}
	}
	raw, present := fields["maintenance"]
	if !present {
		return nil // An old live endpoint is permitted only after durable clear proof.
	}
	var leases []control.MaintenanceResult
	if json.Unmarshal(raw, &leases) != nil || leases == nil {
		return control.ErrMaintenanceInvalid
	}
	for i := range leases {
		if err := (&control.Response{OK: true, Maintenance: &leases[i]}).Err(); err != nil || leases[i].State == control.MaintenanceReleased {
			return control.ErrMaintenanceInvalid
		}
	}
	if len(leases) > 0 {
		return &control.MaintenanceError{Code: control.ErrMaintenanceHeld.Code, Result: &leases[0]}
	}
	return nil
}
