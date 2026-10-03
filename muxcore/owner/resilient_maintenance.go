package owner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/jsonrpc"
)

func isMaintenanceFence(err error) bool {
	return errors.Is(err, control.ErrMaintenanceHeld) || errors.Is(err, control.ErrMaintenanceRetirementBlocked) || errors.Is(err, control.ErrMaintenancePersistenceFailed)
}

func maintenanceWireResult(data []byte) (*control.MaintenanceResult, bool) {
	var frame struct {
		Error struct {
			Code int `json:"code"`
			Data struct {
				ErrorCode   string                     `json:"error_code"`
				Maintenance *control.MaintenanceResult `json:"maintenance"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &frame) != nil || frame.Error.Code != -32005 || frame.Error.Data.ErrorCode != "maintenance_held" {
		return nil, false
	}
	return frame.Error.Data.Maintenance, true
}

func (rc *resilientClient) noteDequeued() {
	rc.localWork.Add(-1)
	select {
	case rc.queueSpace <- struct{}{}:
	default:
	}
}

func (rc *resilientClient) failMaintenanceFrameLocked(data []byte, result ...*control.MaintenanceResult) error {
	id := extractRequestID(data)
	if id == "" {
		return nil
	}
	held := rc.heldResult
	if len(result) != 0 {
		held = result[0]
	}
	if rc.outputMu != nil {
		rc.outputMu.Lock()
		defer rc.outputMu.Unlock()
	}
	_, err := fmt.Fprintf(rc.cfg.Stdout, "%s\n", maintenanceErrorBytes(json.RawMessage(id), held))
	if err != nil {
		rc.stdoutOnce.Do(func() { close(rc.stdoutDead) })
	}
	return err
}

func (rc *resilientClient) drainMaintenanceBufferLocked() {
	for {
		select {
		case data := <-rc.msgFromCC:
			rc.noteDequeued()
			_ = rc.failMaintenanceFrameLocked(data)
		default:
			return
		}
	}
}

func (rc *resilientClient) enterMaintenance(err error) {
	rc.ingressMu.Lock()
	defer rc.ingressMu.Unlock()
	rc.held = true
	rc.maintenanceSeen = true
	var refusal *control.MaintenanceError
	if errors.As(err, &refusal) && refusal.Result != nil {
		copy := *refusal.Result
		rc.heldResult = &copy
	}
	rc.maintenanceSequence++
	rc.initCache.mu.Lock()
	rc.initCache.request = nil
	rc.initCache.requestID = ""
	rc.initCache.mu.Unlock()
	rc.drainMaintenanceBufferLocked()
}

func (rc *resilientClient) leaveMaintenance() {
	rc.ingressMu.Lock()
	defer rc.ingressMu.Unlock()
	// Drain before opening ingress, even if reconnect and host read are both ready.
	// Held frames cannot be handed to the next connection by select ordering.
	if rc.held {
		rc.drainMaintenanceBufferLocked()
	}
	rc.held = false
	rc.heldResult = nil
}

func (rc *resilientClient) maintenanceObserved() bool {
	rc.ingressMu.Lock()
	defer rc.ingressMu.Unlock()
	return rc.maintenanceSeen
}

func (rc *resilientClient) rejectReconnectWake(wake []*[]byte, result ...*control.MaintenanceResult) {
	rc.ingressMu.Lock()
	defer rc.ingressMu.Unlock()
	if !rc.held && len(result) == 0 {
		return
	}
	for _, frame := range wake {
		if frame != nil && *frame != nil {
			_ = rc.failMaintenanceFrameLocked(*frame, result...)
			*frame = nil
			rc.noteDequeued()
		}
	}
}

// Serialize control admission with background reconnect. A demand admitted after
// resume owns the same successor token/path the proxy will connect, not a second
// isolated Spawn. Typed refusals are applied before another demand can recheck.
func (rc *resilientClient) attemptReconnect(fn ReconnectFunc) reconnectResult {
	rc.reconnectMu.Lock()
	defer rc.reconnectMu.Unlock()
	return rc.attemptReconnectLocked(fn)
}

func (rc *resilientClient) attemptReconnectLocked(fn ReconnectFunc) reconnectResult {
	if pending := rc.pendingReconnect.Load(); pending != nil {
		return *pending
	}
	path, token, err := fn()
	res := reconnectResult{path: path, token: token, err: err}
	if isMaintenanceFence(err) {
		rc.enterMaintenance(err)
	} else if err == nil && rc.maintenanceObserved() {
		rc.pendingReconnect.Store(&res)
	}
	return res
}

func (rc *resilientClient) recheckMaintenance() (uint64, bool) {
	rc.reconnectMu.Lock()
	defer rc.reconnectMu.Unlock()
	rc.ingressMu.Lock()
	held, sequence := rc.held, rc.maintenanceSequence
	rc.ingressMu.Unlock()
	if !held {
		return sequence, true
	}
	fn := rc.cfg.Reconnect
	if fn == nil {
		fn = rc.cfg.RefreshToken
	}
	if fn == nil || rc.attemptReconnectLocked(fn).err != nil {
		return sequence, false
	}
	// Only this newly admitted demand may enter the queue. Pre-fence capacity
	// waiters retain their old sequence and can never cross into the successor.
	rc.leaveMaintenance()
	rc.ingressMu.Lock()
	sequence = rc.maintenanceSequence
	rc.ingressMu.Unlock()
	return sequence, true
}

func (rc *resilientClient) enqueueHostFrame(data []byte, msg *jsonrpc.Message) error {
	rc.ingressMu.Lock()
	sequence, held := rc.maintenanceSequence, rc.held
	rc.ingressMu.Unlock()
	// The local held bit is an observation, not the daemon's current lease.
	// Revalidate this demand before rejecting it after an explicit resume.
	if held {
		if admittedSequence, admitted := rc.recheckMaintenance(); admitted {
			sequence = admittedSequence
		}
	}
	for {
		rc.ingressMu.Lock()
		if rc.held || sequence != rc.maintenanceSequence {
			err := rc.failMaintenanceFrameLocked(data)
			rc.ingressMu.Unlock()
			return err
		}
		rc.suspendMu.Lock()
		rc.lastHostActivity.Store(time.Now().UnixNano())
		rc.localWork.Add(1)
		select {
		case rc.msgFromCC <- data:
			if rc.cfg.ProtocolEra != era.EraModern20260728 && !rc.maintenanceSeen && msg.IsRequest() && msg.Method == "initialize" {
				rc.initCache.mu.Lock()
				if rc.initCache.request == nil {
					rc.initCache.request = append([]byte(nil), data...)
					rc.initCache.requestID = string(msg.ID)
				}
				rc.initCache.mu.Unlock()
			}
			rc.suspendMu.Unlock()
			rc.ingressMu.Unlock()
			return nil
		default:
			rc.localWork.Add(-1)
			rc.suspendMu.Unlock()
			rc.ingressMu.Unlock()
		}
		// Backpressure, never drop an original request ID at queue capacity.
		select {
		case <-rc.queueSpace:
		case <-rc.stdoutDead:
			return io.ErrClosedPipe
		case <-rc.transportDone:
			return io.EOF
		}
	}
}
