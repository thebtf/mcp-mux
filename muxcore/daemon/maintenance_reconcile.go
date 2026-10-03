package daemon

import (
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

// Reuse the existing reaper and lease timer; never invent process authority.
func (d *Daemon) reconcileMaintenance() {
	d.maintenanceGate.RLock()
	active := len(d.maintenanceLeases) > 0
	d.maintenanceGate.RUnlock()
	if !active || d.maintenanceLockPath == "" {
		return
	}
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		select {
		case <-d.done:
			return
		default:
		}
		time.AfterFunc(100*time.Millisecond, d.reconcileMaintenance)
		return
	}
	defer lock.Close()
	d.maintenanceGate.Lock()
	defer d.maintenanceGate.Unlock()
	if d.maintenanceFailed {
		return
	}
	for _, lease := range d.maintenanceLeases {
		if lease.result.State != control.MaintenanceHeld && d.maintenanceTreesRetiredLocked(lease) {
			if err := d.finishMaintenanceRetirementLocked(lease); err != nil {
				return
			}
		}
	}
	_ = d.expireMaintenanceLocked(time.Now())
}

func (d *Daemon) maintenanceStatusCode() control.MaintenanceErrorCode {
	d.maintenanceGate.RLock()
	defer d.maintenanceGate.RUnlock()
	if d.maintenanceFailed {
		return control.ErrMaintenancePersistenceFailed.Code
	}
	return ""
}

func (d *Daemon) checkMaintenanceEntryLocked(entry *OwnerEntry) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if entry == nil {
		return control.ErrMaintenanceNotFound
	}
	for key := range entry.maintenanceContexts {
		if err := d.checkMaintenanceKeyLocked(key); err != nil {
			return err
		}
	}
	return nil
}
