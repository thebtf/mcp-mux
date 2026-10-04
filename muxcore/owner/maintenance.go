package owner

import (
	"encoding/json"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
	"github.com/thebtf/mcp-mux/muxcore/upstream"
)

// SetMaintenance publishes an immutable fence while the daemon holds its gate.
// It does no transport I/O and never reacquires the gate.
func (o *Owner) SetMaintenance(result *control.MaintenanceResult) {
	if result == nil {
		o.maintenance.Store(nil)
		return
	}
	if result.State == control.MaintenanceReleased {
		o.maintenance.Store(nil)
		return
	}
	copy := *result
	o.maintenance.Store(&copy)
}

// MaintenanceRetired is stronger than handoff-capable owner completion.
func (o *Owner) MaintenanceRetired() bool {
	return o.maintenanceRetired.Load() && o.nativeQuiescent()
}

// nativeAdmissionClosed is checked while admission is serialized with listener
// teardown. A maintenance read lease precedes admissionMu and mu, never user code.
func (o *Owner) nativeAdmissionClosed() bool {
	if o.maintenance.Load() != nil {
		return true
	}
	select {
	case <-o.listenerDone:
		return true
	case <-o.done:
		return true
	default:
		return false
	}
}

func (o *Owner) reserveNativeWork() bool {
	o.lockRequestAdmission()
	defer o.unlockRequestAdmission()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.nativeAdmissionClosed() {
		return false
	}
	o.nativeWork.Add(1)
	return true
}

// Callback-capable readers remain in sessions through teardown. Callback work
// retains its reservation until actual return, not a verdict timeout. Disconnect
// reserves under mu before unlinking, so this snapshot cannot miss a producer.
func (o *Owner) nativeQuiescent() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.nativeAdmissionClosed() && len(o.sessions) == 0 && o.nativeWork.Load() == 0 && o.PendingRequests() == 0
}

// CurrentLaunchContext returns the exact last elected process context.
func (o *Owner) CurrentLaunchContext() LaunchContext {
	o.materializationMu.Lock()
	defer o.materializationMu.Unlock()
	launch := o.lastMaintenanceLaunch
	if launch.Env == nil && launch.Cwd == "" {
		return o.launchContextForSession(nil)
	}
	return LaunchContext{Cwd: launch.Cwd, Env: cloneLaunchEnv(launch.Env)}
}

func maintenanceErrorBytes(id json.RawMessage, result *control.MaintenanceResult) []byte {
	data := struct {
		ErrorCode   string                     `json:"error_code"`
		Maintenance *control.MaintenanceResult `json:"maintenance,omitempty"`
	}{"maintenance_held", result}
	payload, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	}{-32005, "upstream held for update", data}})
	return payload
}

func (o *Owner) rejectMaintenance(s *Session, msg *jsonrpc.Message, held *control.MaintenanceResult) error {
	if !msg.IsRequest() {
		return nil
	}
	return s.WriteRaw(maintenanceErrorBytes(msg.ID, held))
}

// Request admission ends after local reservation and generation binding, before
// transport I/O. Retirement can then interrupt an already-admitted writer.
func (o *Owner) lockRequestAdmission() *control.MaintenanceResult {
	if o.maintenanceGate != nil {
		o.maintenanceGate.RLock()
	}
	return o.maintenance.Load()
}

func (o *Owner) unlockRequestAdmission() {
	if o.maintenanceGate != nil {
		o.maintenanceGate.RUnlock()
	}
}

// DrainForMaintenance gives already-reserved generation work its accepted
// deadline, including a blocked write. Unreserved queued demand is never replayed.
func (o *Owner) DrainForMaintenance(deadline time.Time) {
	demands := o.detachAllLocalDemands()
	for _, demand := range demands {
		_ = demand.session.WriteRaw(maintenanceErrorBytes(demand.message.ID, o.maintenance.Load()))
	}
	o.DrainRequestsUntil(deadline)
	o.drainInflightRequests()
}

// DrainRequestsUntil keeps the original deadline. Only maintenance also drains
// native non-request callbacks; ordinary restart remains request-only.
func (o *Owner) DrainRequestsUntil(deadline time.Time) {
	for (o.PendingRequests() > 0 || (o.maintenance.Load() != nil && o.nativeWork.Load() > 0)) && time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > 10*time.Millisecond {
			remaining = 10 * time.Millisecond
		}
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-o.done:
			timer.Stop()
			return
		}
	}
}

// startAdmittedMaterialization holds the managed admission lease across final
// election, physical start and installation, including a partial failed start.
func (o *Owner) startAdmittedMaterialization(a *materializationAttempt) (*upstream.Process, *materializationSignals, error) {
	if o.maintenanceGate != nil {
		o.maintenanceGate.RLock()
		defer o.maintenanceGate.RUnlock()
	}
	if held := o.maintenance.Load(); held != nil {
		return nil, nil, &control.MaintenanceError{Code: control.ErrMaintenanceHeld.Code, Result: held}
	}
	o.launchContextMu.Lock()
	defer o.launchContextMu.Unlock()
	o.materializationMu.Lock()
	if !a.launchFrozen {
		if !o.launchSessionEligible(a.launch) {
			if a.launchRequiresSession {
				if replacement, ok := o.oldestEligibleLaunchContext(); ok {
					a.launch = replacement
				}
			} else {
				a.launch = o.electLaunchContextLocked()
			}
		}
		a.launchFrozen = true
	}
	if !o.materializationEraMatches(a) {
		o.materializationMu.Unlock()
		return nil, nil, errMaterializationEraChanged
	}
	launch := LaunchContext{SessionID: a.launch.SessionID, Cwd: a.launch.Cwd, Env: cloneLaunchEnv(a.launch.Env)}
	o.materializationMu.Unlock()
	if o.admitMaterialization != nil {
		if err := o.admitMaterialization(o, launch); err != nil {
			return nil, nil, err
		}
	}
	proc, err := o.spawnReplacementUpstream(launch)
	var signals *materializationSignals
	if proc != nil {
		signals = o.installMaterializationProcess(a, proc)
	}
	return proc, signals, err
}
