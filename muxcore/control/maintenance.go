package control

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaintenanceState is the public state of a daemon-owned maintenance lease.
type MaintenanceState string

const (
	MaintenanceHolding           MaintenanceState = "HOLDING"
	MaintenanceHeld              MaintenanceState = "HELD"
	MaintenanceRetirementBlocked MaintenanceState = "RETIREMENT_BLOCKED"
	MaintenanceReleased          MaintenanceState = "RELEASED"
)

// MaintenanceResult contains only safe lease identifiers and retirement state.
type MaintenanceResult struct {
	HoldID        string           `json:"hold_id"`
	ServerID      string           `json:"server_id"`
	State         MaintenanceState `json:"state"`
	ExpiresAt     time.Time        `json:"expires_at"`
	DrainDeadline time.Time        `json:"drain_deadline"`
	TreesRetired  bool             `json:"trees_retired"`
}

// MaintenanceHandler optionally implements durable hold, resume, and renew.
type MaintenanceHandler interface {
	HandleMaintenance(req Request) (MaintenanceResult, error)
}

// ShutdownWithErrorHandler optionally refuses shutdown without invoking the
// legacy shutdown path.
type ShutdownWithErrorHandler interface {
	HandleShutdownWithError(drainTimeoutMs int) (string, error)
}

// OwnerRestartHandler restarts from daemon-owned context, never adapter inputs.
type OwnerRestartHandler interface {
	HandleRestartOwner(req Request) (Response, error)
}

// MaintenanceErrorCode is a stable control-plane refusal code.
type MaintenanceErrorCode string

var (
	ErrMaintenanceHeld              = &MaintenanceError{Code: "maintenance_held"}
	ErrMaintenanceConflict          = &MaintenanceError{Code: "maintenance_conflict"}
	ErrMaintenanceNotFound          = &MaintenanceError{Code: "maintenance_not_found"}
	ErrMaintenanceRetirementBlocked = &MaintenanceError{Code: "maintenance_retirement_blocked"}
	ErrMaintenanceUnsupported       = &MaintenanceError{Code: "maintenance_unsupported"}
	ErrMaintenancePersistenceFailed = &MaintenanceError{Code: "maintenance_persistence_failed"}
	ErrMaintenanceInvalid           = &MaintenanceError{Code: "maintenance_invalid"}
)

// MaintenanceError preserves typed refusals and safe readback through wrapping.
type MaintenanceError struct {
	Code   MaintenanceErrorCode
	Result *MaintenanceResult
}

func (e *MaintenanceError) Error() string {
	if e == nil {
		return "invalid maintenance request or response"
	}
	switch e.Code {
	case "maintenance_held":
		return "upstream held for update"
	case "maintenance_conflict":
		return "maintenance lease conflict"
	case "maintenance_not_found":
		return "maintenance target or lease not found"
	case "maintenance_retirement_blocked":
		return "maintenance retirement blocked"
	case "maintenance_unsupported":
		return "maintenance not supported"
	case "maintenance_persistence_failed":
		return "maintenance persistence failed"
	default:
		return "invalid maintenance request or response"
	}
}

func (e *MaintenanceError) Is(target error) bool {
	other, ok := target.(*MaintenanceError)
	return ok && e != nil && other != nil && e.Code == other.Code
}

func validMaintenanceCode(code MaintenanceErrorCode) bool {
	switch code {
	case "maintenance_held", "maintenance_conflict", "maintenance_not_found",
		"maintenance_retirement_blocked", "maintenance_unsupported",
		"maintenance_persistence_failed", "maintenance_invalid":
		return true
	default:
		return false
	}
}

// Err classifies typed refusals without trusting a peer's error message.
// Ordinary unsuccessful responses retain their existing message semantics.
func (r *Response) Err() error {
	if r == nil || (r.ErrorCode != "" && (!validMaintenanceCode(r.ErrorCode) || r.OK)) {
		return ErrMaintenanceInvalid
	}
	if r.Maintenance != nil && !validMaintenanceResult(r.Maintenance) {
		return ErrMaintenanceInvalid
	}
	if r.OK {
		return nil
	}
	if r.ErrorCode != "" {
		return &MaintenanceError{Code: r.ErrorCode, Result: r.Maintenance}
	}
	if r.Message != "" {
		return errors.New(r.Message)
	}
	return errors.New("control request failed")
}

func exactMaintenanceID(id string) bool {
	return id != "" && strings.TrimSpace(id) == id
}

func prepareMaintenanceRequest(req Request) (Request, error) {
	if req.DrainTimeoutMs < 0 || req.Command != "" {
		return req, ErrMaintenanceInvalid
	}
	switch req.Cmd {
	case "hold":
		if !exactMaintenanceID(req.ServerID) || req.HoldID != "" {
			return req, ErrMaintenanceInvalid
		}
	case "resume", "renew":
		if !exactMaintenanceID(req.HoldID) || req.ServerID != "" {
			return req, ErrMaintenanceInvalid
		}
	default:
		return req, ErrMaintenanceInvalid
	}
	if req.HoldTTLMS != nil {
		if *req.HoldTTLMS <= 0 || *req.HoldTTLMS > 3600000 {
			return req, ErrMaintenanceInvalid
		}
	} else if req.Cmd != "resume" {
		ttl := int64(300000)
		req.HoldTTLMS = &ttl
	}
	return req, nil
}

func validMaintenanceResult(result *MaintenanceResult) bool {
	if result == nil || !exactMaintenanceID(result.HoldID) || result.ExpiresAt.IsZero() || result.DrainDeadline.IsZero() {
		return false
	}
	// Recovery may retain the lease without its original display server ID.
	if result.ServerID != "" && !exactMaintenanceID(result.ServerID) {
		return false
	}
	switch result.State {
	case MaintenanceHeld, MaintenanceReleased:
		return result.TreesRetired
	case MaintenanceHolding:
		return true
	case MaintenanceRetirementBlocked:
		return !result.TreesRetired
	default:
		return false
	}
}

func maintenanceResponse(req Request, resp *Response) (*MaintenanceResult, error) {
	if err := resp.Err(); err != nil {
		if resp != nil && !resp.OK && resp.ErrorCode == "" && !errors.Is(err, ErrMaintenanceInvalid) {
			return nil, ErrMaintenanceUnsupported
		}
		var maintenanceErr *MaintenanceError
		if errors.As(err, &maintenanceErr) {
			return maintenanceErr.Result, err
		}
		return nil, err
	}
	result := resp.Maintenance
	if result == nil {
		return nil, ErrMaintenanceUnsupported
	}
	valid := false
	switch req.Cmd {
	case "hold":
		valid = result.ServerID == req.ServerID && result.State == MaintenanceHeld && result.ExpiresAt.After(time.Now())
	case "resume":
		valid = result.HoldID == req.HoldID && result.State == MaintenanceReleased
	case "renew":
		valid = result.HoldID == req.HoldID && result.State != MaintenanceReleased && result.ExpiresAt.After(time.Now())
	}
	if !valid {
		return nil, ErrMaintenanceInvalid
	}
	return result, nil
}

// errorResponse preserves typed refusals while retaining legacy error text for
// ordinary errors. Wrapped internal text never enters a maintenance response.
func errorResponse(cmd string, err error) Response {
	var maintenanceErr *MaintenanceError
	if errors.As(err, &maintenanceErr) {
		if maintenanceErr == nil || !validMaintenanceCode(maintenanceErr.Code) ||
			(maintenanceErr.Result != nil && !validMaintenanceResult(maintenanceErr.Result)) {
			maintenanceErr = ErrMaintenanceInvalid
		}
		return Response{Message: maintenanceErr.Error(), ErrorCode: maintenanceErr.Code, Maintenance: maintenanceErr.Result}
	}
	if cmd == "" {
		return Response{Message: err.Error()}
	}
	return Response{Message: fmt.Sprintf("%s: %v", cmd, err)}
}

func (s *Server) dispatchMaintenance(req Request) Response {
	req, err := prepareMaintenanceRequest(req)
	if err != nil {
		return errorResponse("", err)
	}
	handler, ok := s.handler.(MaintenanceHandler)
	if !ok {
		return errorResponse("", ErrMaintenanceUnsupported)
	}
	result, err := handler.HandleMaintenance(req)
	if err != nil {
		resp := errorResponse("", err)
		if resp.ErrorCode == "" {
			return errorResponse("", ErrMaintenanceUnsupported)
		}
		if resp.Maintenance == nil && result.HoldID != "" {
			if !validMaintenanceResult(&result) {
				return errorResponse("", ErrMaintenanceInvalid)
			}
			resp.Maintenance = &result
		}
		return resp
	}
	resp := Response{OK: true, Maintenance: &result}
	if _, err := maintenanceResponse(req, &resp); err != nil {
		return errorResponse("", err)
	}
	return resp
}
