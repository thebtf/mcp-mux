package daemon

import (
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
)

func (d *Daemon) maintenanceRestoreContexts(snap mcpsnapshot.OwnerSnapshot) (map[string]bool, bool) {
	contexts := map[string]bool{d.maintenanceContext(era.EraLegacy, snap.Command, snap.Args, snap.Cwd, snap.Env): true}
	knownCWD := map[string]bool{serverid.CanonicalizePath(snap.Cwd): true}
	for _, token := range snap.BoundTokens {
		if token.OwnerKey != snap.ServerID {
			return contexts, false
		}
		contexts[d.maintenanceContext(era.EraLegacy, snap.Command, snap.Args, token.Cwd, token.Env)] = true
		knownCWD[serverid.CanonicalizePath(token.Cwd)] = true
	}
	for _, cwd := range snap.CwdSet {
		if !knownCWD[serverid.CanonicalizePath(cwd)] {
			return contexts, false
		}
	}
	return contexts, true
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
