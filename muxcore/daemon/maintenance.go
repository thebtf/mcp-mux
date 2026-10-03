package daemon

import (
	"sort"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/owner"
)

type maintenancePin struct {
	entry    *OwnerEntry
	identity ownerEntryIdentity
	creating <-chan struct{}
}
type maintenanceLease struct {
	record    maintenanceRecord
	result    control.MaintenanceResult
	pins      []maintenancePin
	recovered bool
}

func maintenanceFailure(code *control.MaintenanceError, lease *maintenanceLease) error {
	failure := &control.MaintenanceError{Code: code.Code}
	if lease != nil {
		result := lease.result
		failure.Result = &result
	}
	return failure
}

func copyMaintenanceLeases(src map[string]*maintenanceLease) map[string]*maintenanceLease {
	dst := make(map[string]*maintenanceLease, len(src))
	for id, lease := range src {
		dst[id] = lease
	}
	return dst
}

func contextsOverlap(keys []string, contexts map[string]bool) bool {
	for _, key := range keys {
		if contexts[key] {
			return true
		}
	}
	return false
}

func (d *Daemon) maintenanceForKeyLocked(key string) *maintenanceLease {
	for _, lease := range d.maintenanceLeases {
		for _, candidate := range lease.record.Keys {
			if candidate == key {
				return lease
			}
		}
	}
	return nil
}

func (d *Daemon) checkMaintenanceKeyLocked(key string) error {
	if d.maintenanceFailed {
		return control.ErrMaintenancePersistenceFailed
	}
	if lease := d.maintenanceForKeyLocked(key); lease != nil {
		return maintenanceFailure(control.ErrMaintenanceHeld, lease)
	}
	return nil
}

func (d *Daemon) maintenanceAdmission(key string) error {
	d.maintenanceGate.Lock()
	defer d.maintenanceGate.Unlock()
	return d.checkMaintenanceKeyLocked(key)
}

// lockMaintenanceRegistry always acquires admission before registry locks.
func (d *Daemon) lockMaintenanceRegistry(key string) error {
	d.maintenanceGate.RLock()
	if err := d.checkMaintenanceKeyLocked(key); err != nil {
		d.maintenanceGate.RUnlock()
		return err
	}
	d.mu.Lock()
	return nil
}
func (d *Daemon) unlockMaintenanceRegistry() { d.mu.Unlock(); d.maintenanceGate.RUnlock() }

func (d *Daemon) admitOwner(entry *OwnerEntry, token, cwd string, env map[string]string, initial bool) (bool, error) {
	key := d.maintenanceContext(entry.ProtocolEra, entry.Command, entry.Args, cwd, env)
	d.maintenanceGate.RLock()
	defer d.maintenanceGate.RUnlock()
	if err := d.checkMaintenanceKeyLocked(key); err != nil {
		return false, err
	}
	d.mu.Lock()
	if d.owners[entry.ServerID] != entry || entry.Owner == nil {
		d.mu.Unlock()
		return false, nil
	}
	for existing := range entry.maintenanceContexts {
		if err := d.checkMaintenanceKeyLocked(existing); err != nil {
			d.mu.Unlock()
			return false, err
		}
	}
	d.mu.Unlock()
	var admitted bool
	if initial {
		admitted = entry.Owner.PreRegisterInitial(token, cwd, env)
	} else {
		admitted = entry.Owner.PreRegister(token, cwd, env)
	}
	if admitted {
		d.mu.Lock()
		if entry.maintenanceContexts == nil {
			entry.maintenanceContexts = make(map[string]bool)
		}
		entry.maintenanceContexts[key] = true
		d.mu.Unlock()
		entry.Owner.AddCwd(cwd)
	}
	return admitted, nil
}

// admitMaterialization is called under the shared gate, never reacquires it,
// and does not enter owner locks. Actual elected context joins the finite set.
func (d *Daemon) admitMaterialization(o *owner.Owner, launch owner.LaunchContext) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry := d.owners[o.ServerID()]
	if entry == nil || entry.Owner != o {
		return ErrOwnerGone
	}
	if d.shuttingDown.Load() {
		return ErrDaemonShuttingDown
	}
	key := d.maintenanceContext(entry.ProtocolEra, entry.Command, entry.Args, launch.Cwd, launch.Env)
	if err := d.checkMaintenanceKeyLocked(key); err != nil {
		return err
	}
	for existing := range entry.maintenanceContexts {
		if err := d.checkMaintenanceKeyLocked(existing); err != nil {
			return err
		}
	}
	if entry.maintenanceContexts == nil {
		entry.maintenanceContexts = make(map[string]bool)
	}
	entry.maintenanceContexts[key] = true
	return nil
}

func (d *Daemon) maintenanceLifecycleLocked() error {
	if d.maintenanceFailed {
		return control.ErrMaintenancePersistenceFailed
	}
	if len(d.maintenanceLeases) > 0 {
		return control.ErrMaintenanceHeld
	}
	return nil
}

func (d *Daemon) beginMaintenanceLifecycle() error {
	d.maintenanceGate.Lock()
	defer d.maintenanceGate.Unlock()
	if err := d.maintenanceLifecycleLocked(); err != nil {
		return err
	}
	d.shuttingDown.Store(true)
	return nil
}

func (d *Daemon) maintenanceFenced() bool {
	d.maintenanceGate.Lock()
	defer d.maintenanceGate.Unlock()
	return d.maintenanceLifecycleLocked() != nil
}

func (d *Daemon) HandleShutdownWithError(drainTimeoutMs int) (string, error) {
	if err := d.beginMaintenanceLifecycle(); err != nil {
		return "", err
	}
	go d.shutdown(nil)
	return "daemon shutting down", nil
}

func (d *Daemon) HandleMaintenance(req control.Request) (control.MaintenanceResult, error) {
	if req.DrainTimeoutMs < 0 || int64(req.DrainTimeoutMs) > int64((1<<63-1)/time.Millisecond) || req.Command != "" {
		return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
	}
	ttl := 5 * time.Minute
	if req.HoldTTLMS != nil {
		if *req.HoldTTLMS <= 0 || *req.HoldTTLMS > 3600000 {
			return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
		}
		ttl = time.Duration(*req.HoldTTLMS) * time.Millisecond
	}
	switch req.Cmd {
	case "hold":
		if req.ServerID == "" || strings.TrimSpace(req.ServerID) != req.ServerID || req.HoldID != "" {
			return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
		}
		return d.acquireMaintenance(req, ttl)
	case "resume", "renew":
		if req.HoldID == "" || strings.TrimSpace(req.HoldID) != req.HoldID || req.ServerID != "" {
			return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
		}
		return d.mutateMaintenance(req, ttl)
	default:
		return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
	}
}

func (d *Daemon) acquireMaintenance(req control.Request, ttl time.Duration) (control.MaintenanceResult, error) {
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		return control.MaintenanceResult{}, control.ErrMaintenancePersistenceFailed
	}
	defer lock.Close()
	d.maintenanceGate.Lock()
	if err := d.expireMaintenanceLocked(time.Now()); err != nil {
		d.maintenanceGate.Unlock()
		return control.MaintenanceResult{}, err
	}
	if d.maintenanceFailed {
		d.maintenanceGate.Unlock()
		return control.MaintenanceResult{}, control.ErrMaintenancePersistenceFailed
	}
	if d.shuttingDown.Load() {
		d.maintenanceGate.Unlock()
		return control.MaintenanceResult{}, control.ErrMaintenanceHeld
	}
	for _, lease := range d.maintenanceLeases {
		if lease.result.ServerID == req.ServerID {
			result := lease.result
			d.maintenanceGate.Unlock()
			return result, maintenanceFailure(control.ErrMaintenanceConflict, lease)
		}
	}
	d.mu.RLock()
	selected := d.owners[req.ServerID]
	if selected == nil {
		d.mu.RUnlock()
		d.maintenanceGate.Unlock()
		return control.MaintenanceResult{}, control.ErrMaintenanceNotFound
	}
	if len(selected.maintenanceContexts) == 0 || selected.maintenanceIncomplete {
		d.mu.RUnlock()
		d.maintenanceGate.Unlock()
		return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
	}
	keys := make([]string, 0, len(selected.maintenanceContexts))
	for key := range selected.maintenanceContexts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pins := make([]maintenancePin, 0)
	for _, entry := range d.owners {
		if entry.Command == selected.Command && argsEqual(entry.Args, selected.Args) && (len(entry.maintenanceContexts) == 0 || entry.maintenanceIncomplete) {
			d.mu.RUnlock()
			d.maintenanceGate.Unlock()
			return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
		}
		if !contextsOverlap(keys, entry.maintenanceContexts) {
			continue
		}
		for key := range entry.maintenanceContexts {
			if !selected.maintenanceContexts[key] {
				d.mu.RUnlock()
				d.maintenanceGate.Unlock()
				return control.MaintenanceResult{}, control.ErrMaintenanceInvalid
			}
		}
		pins = append(pins, maintenancePin{entry: entry, identity: captureOwnerEntryIdentity(entry), creating: entry.creating})
	}
	d.mu.RUnlock()
	for _, key := range keys {
		if lease := d.maintenanceForKeyLocked(key); lease != nil {
			result := lease.result
			d.maintenanceGate.Unlock()
			return result, maintenanceFailure(control.ErrMaintenanceConflict, lease)
		}
	}
	id, err := generateToken()
	if err != nil {
		d.maintenanceGate.Unlock()
		return control.MaintenanceResult{}, control.ErrMaintenancePersistenceFailed
	}
	now := time.Now().UTC()
	lease := &maintenanceLease{record: maintenanceRecord{HoldID: id, Keys: keys}, pins: pins, result: control.MaintenanceResult{HoldID: id, ServerID: req.ServerID, State: control.MaintenanceHolding, ExpiresAt: now.Add(ttl), DrainDeadline: now.Add(time.Duration(req.DrainTimeoutMs) * time.Millisecond)}}
	next := copyMaintenanceLeases(d.maintenanceLeases)
	next[id] = lease
	if err := d.persistMaintenanceLocked(next); err != nil {
		d.maintenanceFailed = true
		d.mu.RLock()
		for _, entry := range d.owners {
			if entry.Owner != nil {
				entry.Owner.SetMaintenance(&lease.result)
			}
		}
		d.mu.RUnlock()
		d.maintenanceGate.Unlock()
		return lease.result, maintenanceFailure(control.ErrMaintenancePersistenceFailed, lease)
	}
	d.maintenanceLeases = next
	for _, pin := range pins {
		if pin.entry.Owner != nil {
			pin.entry.Owner.SetMaintenance(&lease.result)
		}
	}
	d.scheduleMaintenanceExpiryLocked(lease)
	d.maintenanceGate.Unlock()

	// A placeholder has no process before promotion. Promotion rechecks the fence,
	// settles its channel and discards the inert owner before any start is admitted.
	blocked := false
	for _, pin := range pins {
		if pin.creating != nil {
			timer := time.NewTimer(time.Until(lease.result.DrainDeadline))
			select {
			case <-pin.creating:
				timer.Stop()
			case <-timer.C:
				blocked = true
			}
		}
	}
	for _, pin := range pins {
		if pin.entry.Owner != nil {
			pin.entry.Owner.DrainForMaintenance(lease.result.DrainDeadline)
		}
	}
	for _, pin := range pins {
		if pin.entry.Owner == nil {
			continue
		}
		if _, err := d.removeOwnerIfCurrent(pin.identity.serverID, pin.entry, ownerRemovalReasonMaintenance, false); err != nil && !pin.entry.Owner.MaintenanceRetired() {
			d.scheduleOwnerFinalizationRetry(pin.identity.serverID, pin.entry, ownerRemovalReasonMaintenance, false)
			blocked = true
		}
	}
	if blocked {
		return d.blockMaintenance(lease)
	}
	d.maintenanceGate.Lock()
	defer d.maintenanceGate.Unlock()
	current := d.maintenanceLeases[id]
	if current == nil {
		result := lease.result
		result.State = control.MaintenanceReleased
		result.TreesRetired = true
		return result, control.ErrMaintenanceConflict
	}
	if !d.maintenanceTreesRetiredLocked(current) {
		return current.result, maintenanceFailure(control.ErrMaintenanceRetirementBlocked, current)
	}
	if err := d.finishMaintenanceRetirementLocked(current); err != nil {
		return current.result, maintenanceFailure(control.ErrMaintenancePersistenceFailed, current)
	}
	current = d.maintenanceLeases[id]
	if current == nil {
		result := lease.result
		result.State = control.MaintenanceReleased
		result.TreesRetired = true
		return result, maintenanceFailure(control.ErrMaintenanceConflict, nil)
	}
	return current.result, nil
}

func (d *Daemon) maintenanceTreesRetiredLocked(lease *maintenanceLease) bool {
	if lease.recovered {
		return lease.result.State == control.MaintenanceHeld
	}
	for _, pin := range lease.pins {
		if !pin.identity.matches(pin.entry) {
			return false
		}
		if pin.creating != nil {
			select {
			case <-pin.creating:
			default:
				return false
			}
		}
		if pin.entry.Owner != nil && !pin.entry.Owner.MaintenanceRetired() {
			return false
		}
	}
	return true
}

func (d *Daemon) blockMaintenance(lease *maintenanceLease) (control.MaintenanceResult, error) {
	d.maintenanceGate.Lock()
	defer d.maintenanceGate.Unlock()
	current := d.maintenanceLeases[lease.result.HoldID]
	if current == nil {
		return lease.result, control.ErrMaintenanceConflict
	}
	if d.maintenanceTreesRetiredLocked(current) {
		if err := d.finishMaintenanceRetirementLocked(current); err != nil {
			return current.result, maintenanceFailure(control.ErrMaintenancePersistenceFailed, current)
		}
		if held := d.maintenanceLeases[current.result.HoldID]; held != nil {
			return held.result, nil
		}
		result := current.result
		result.State = control.MaintenanceReleased
		result.TreesRetired = true
		return result, control.ErrMaintenanceConflict
	}
	updated := *current
	updated.result.State = control.MaintenanceRetirementBlocked
	updated.result.TreesRetired = false
	if err := d.commitMaintenanceLeaseLocked(current, &updated); err != nil {
		return current.result, maintenanceFailure(control.ErrMaintenancePersistenceFailed, current)
	}
	return updated.result, maintenanceFailure(control.ErrMaintenanceRetirementBlocked, &updated)
}

func (d *Daemon) commitMaintenanceLeaseLocked(current, updated *maintenanceLease) error {
	if d.maintenanceLeases[current.result.HoldID] != current {
		return control.ErrMaintenanceConflict
	}
	next := copyMaintenanceLeases(d.maintenanceLeases)
	if updated.result.State == control.MaintenanceReleased {
		delete(next, current.result.HoldID)
	} else {
		next[current.result.HoldID] = updated
	}
	if err := d.persistMaintenanceLocked(next); err != nil {
		return err
	}
	d.maintenanceLeases = next
	for _, pin := range updated.pins {
		if pin.entry.Owner != nil {
			pin.entry.Owner.SetMaintenance(&updated.result)
		}
	}
	if updated.result.State != control.MaintenanceReleased {
		d.scheduleMaintenanceExpiryLocked(updated)
	}
	return nil
}

func (d *Daemon) finishMaintenanceRetirementLocked(lease *maintenanceLease) error {
	if !d.maintenanceTreesRetiredLocked(lease) {
		return control.ErrMaintenanceRetirementBlocked
	}
	updated := *lease
	updated.result.TreesRetired = true
	updated.result.State = control.MaintenanceHeld
	if !time.Now().Before(updated.result.ExpiresAt) {
		updated.result.State = control.MaintenanceReleased
	}
	return d.commitMaintenanceLeaseLocked(lease, &updated)
}

// maintenanceRetirementChanged is reached by the existing exact-entry retry
// path after registry locks are released. It cannot retire a newer generation.
func (d *Daemon) maintenanceRetirementChanged(entry *OwnerEntry) {
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		return
	}
	defer lock.Close()
	d.maintenanceGate.Lock()
	defer d.maintenanceGate.Unlock()
	for _, lease := range d.maintenanceLeases {
		matched := false
		for _, pin := range lease.pins {
			if pin.entry == entry && pin.identity.matches(entry) {
				matched = true
				break
			}
		}
		if matched && lease.result.State != control.MaintenanceHeld && d.maintenanceTreesRetiredLocked(lease) {
			_ = d.finishMaintenanceRetirementLocked(lease)
		}
	}
}

func (d *Daemon) scheduleMaintenanceExpiryLocked(lease *maintenanceLease) {
	time.AfterFunc(time.Until(lease.result.ExpiresAt), func() {
		lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
		if err != nil {
			time.AfterFunc(100*time.Millisecond, func() { d.reconcileMaintenance() })
			return
		}
		defer lock.Close()
		d.maintenanceGate.Lock()
		defer d.maintenanceGate.Unlock()
		if d.maintenanceLeases[lease.result.HoldID] != lease {
			return
		}
		_ = d.expireMaintenanceLocked(time.Now())
	})
}

func (d *Daemon) expireMaintenanceLocked(now time.Time) error {
	for _, lease := range d.maintenanceLeases {
		if lease.result.State != control.MaintenanceHeld || now.Before(lease.result.ExpiresAt) {
			continue
		}
		updated := *lease
		updated.result.State = control.MaintenanceReleased
		if err := d.commitMaintenanceLeaseLocked(lease, &updated); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) mutateMaintenance(req control.Request, ttl time.Duration) (control.MaintenanceResult, error) {
	lock, err := ipc.AcquireFileLock(d.maintenanceLockPath)
	if err != nil {
		return control.MaintenanceResult{}, control.ErrMaintenancePersistenceFailed
	}
	defer lock.Close()
	d.maintenanceGate.Lock()
	defer d.maintenanceGate.Unlock()
	if err := d.expireMaintenanceLocked(time.Now()); err != nil {
		return control.MaintenanceResult{}, err
	}
	current := d.maintenanceLeases[req.HoldID]
	if current == nil {
		if len(d.maintenanceLeases) > 0 {
			return control.MaintenanceResult{}, control.ErrMaintenanceConflict
		}
		return control.MaintenanceResult{}, control.ErrMaintenanceNotFound
	}
	if d.maintenanceFailed {
		return current.result, maintenanceFailure(control.ErrMaintenancePersistenceFailed, current)
	}
	updated := *current
	if req.Cmd == "resume" {
		if current.result.State != control.MaintenanceHeld || !d.maintenanceTreesRetiredLocked(current) {
			return current.result, maintenanceFailure(control.ErrMaintenanceRetirementBlocked, current)
		}
		updated.result.State = control.MaintenanceReleased
	} else {
		if !time.Now().Before(current.result.ExpiresAt) {
			return current.result, maintenanceFailure(control.ErrMaintenanceConflict, current)
		}
		updated.result.ExpiresAt = time.Now().UTC().Add(ttl)
	}
	if err := d.commitMaintenanceLeaseLocked(current, &updated); err != nil {
		return current.result, maintenanceFailure(control.ErrMaintenancePersistenceFailed, current)
	}
	return updated.result, nil
}

func (d *Daemon) maintenanceResults() []control.MaintenanceResult {
	d.maintenanceGate.RLock()
	defer d.maintenanceGate.RUnlock()
	results := make([]control.MaintenanceResult, 0, len(d.maintenanceLeases))
	for _, lease := range d.maintenanceLeases {
		results = append(results, lease.result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].HoldID < results[j].HoldID })
	return results
}

func (d *Daemon) maintenanceForServer(serverID string) *control.MaintenanceResult {
	for _, result := range d.maintenanceResults() {
		if result.ServerID == serverID {
			return &result
		}
	}
	return nil
}

func (d *Daemon) HandleRestartOwner(req control.Request) (control.Response, error) {
	if req.ServerID == "" || strings.TrimSpace(req.ServerID) != req.ServerID || req.Command != "" || req.DrainTimeoutMs < 0 {
		return control.Response{}, control.ErrMaintenanceInvalid
	}
	d.maintenanceGate.RLock()
	if d.maintenanceFailed {
		d.maintenanceGate.RUnlock()
		return control.Response{}, control.ErrMaintenancePersistenceFailed
	}
	for _, lease := range d.maintenanceLeases {
		if lease.result.ServerID == req.ServerID {
			d.maintenanceGate.RUnlock()
			return control.Response{}, maintenanceFailure(control.ErrMaintenanceHeld, lease)
		}
	}
	d.mu.RLock()
	entry := d.owners[req.ServerID]
	if entry == nil || entry.Owner == nil {
		d.mu.RUnlock()
		d.maintenanceGate.RUnlock()
		return control.Response{}, control.ErrMaintenanceNotFound
	}
	for key := range entry.maintenanceContexts {
		if err := d.checkMaintenanceKeyLocked(key); err != nil {
			d.mu.RUnlock()
			d.maintenanceGate.RUnlock()
			return control.Response{}, err
		}
	}
	d.mu.RUnlock()
	// Capture exactly the elected upstream context, never under registry locks.
	launch := entry.Owner.CurrentLaunchContext()
	wire, _ := entry.ProtocolEra.Wire()
	spawn := control.Request{Cmd: "spawn", Command: entry.Command, Args: append([]string(nil), entry.Args...), Cwd: launch.Cwd, Env: launch.Env, Mode: entry.Mode, ProtocolEra: wire}
	d.maintenanceGate.RUnlock()
	if req.DrainTimeoutMs > 0 {
		entry.Owner.DrainRequestsUntil(time.Now().Add(time.Duration(req.DrainTimeoutMs) * time.Millisecond))
	}
	removed, err := d.removeOwnerIfCurrent(req.ServerID, entry, ownerRemovalReasonOperatorHard, false)
	if err != nil {
		return control.Response{}, err
	}
	if !removed.Removed {
		d.mu.RLock()
		current := d.owners[req.ServerID]
		d.mu.RUnlock()
		if current == nil {
			return control.Response{}, control.ErrMaintenanceNotFound
		}
		return control.Response{}, control.ErrMaintenanceConflict
	}
	path, sid, token, err := d.Spawn(spawn)
	if err != nil {
		return control.Response{}, err
	}
	return control.Response{OK: true, IPCPath: path, ServerID: sid, Token: token, ProtocolEra: wire}, nil
}

var (
	_ control.MaintenanceHandler       = (*Daemon)(nil)
	_ control.ShutdownWithErrorHandler = (*Daemon)(nil)
	_ control.OwnerRestartHandler      = (*Daemon)(nil)
)
