package daemon

import (
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
)

func (d *Daemon) maintenanceRestoreContexts(snap mcpsnapshot.OwnerSnapshot) (map[string]bool, bool) {
	contexts := map[string]bool{d.maintenanceContext(era.EraLegacy, snap.Command, snap.Args, snap.Cwd, snap.Env): true}
	for _, token := range snap.BoundTokens {
		if token.OwnerKey != snap.ServerID {
			return contexts, false
		}
		contexts[d.maintenanceContext(era.EraLegacy, snap.Command, snap.Args, token.Cwd, token.Env)] = true
	}
	// CwdSet retains roots, not their environments, and BoundTokens expires.
	// Neither proves every context admitted before snapshot or handoff, even
	// when every retained token has the same CWD and environment as the base.
	return contexts, false
}

func (d *Daemon) checkMaintenanceRestoreLocked(snap mcpsnapshot.OwnerSnapshot) error {
	contexts, complete := d.maintenanceRestoreContexts(snap)
	for key := range contexts {
		if err := d.checkMaintenanceKeyLocked(key); err != nil {
			return err
		}
	}
	if !complete && len(d.maintenanceLeases) > 0 {
		return control.ErrMaintenanceInvalid
	}
	return nil
}
