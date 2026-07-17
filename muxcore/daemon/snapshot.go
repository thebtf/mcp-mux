package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/classify"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
	mcpsnapshot "github.com/thebtf/mcp-mux/muxcore/snapshot"
	"github.com/thejerf/suture/v4"
)

// retrySidPattern matches forced-isolated retry sids of the form
// `isolated-<hex16>-r<N>`. Used on snapshot load to rehydrate the in-memory
// forcedIsolatedRetryCounters so a fresh Spawn for the same (cmd,args,cwd)
// after restart computes the same `-rN` suffix instead of creating a
// duplicate owner under the base `isolated-<hex16>` sid (codex PR #121 finding).
var retrySidPattern = regexp.MustCompile(`^(isolated-[0-9a-f]+)-r(\d+)$`)

// rehydrateRetryCounter parses a retry-suffixed sid, computes the base
// isolated identity for the (cmd,args,cwd), and bumps the in-memory counter
// to at least N so the next forced-isolated retry produces `-r<N+1>` rather
// than colliding with the restored owner's sid.
//
// Uses a CAS loop because multiple snapshot entries may share the same base
// (e.g. -r1 and -r2 both restored) and a plain Store would let a smaller N
// race ahead of a larger one.
//
// The cmd/args/cwd are re-derived to confirm the parsed base matches the
// deterministic CR-001 hash; if it does not (snapshot from a different
// scheme version, or hand-crafted sid), the counter is updated against the
// recomputed base instead. This keeps the retry suffix consistent with what
// a fresh Spawn would compute, not with whatever happened to be in the
// snapshot's literal sid field.
func (d *Daemon) rehydrateRetryCounter(sid, cmd string, args []string, cwd string) {
	m := retrySidPattern.FindStringSubmatch(sid)
	if m == nil {
		return
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || n <= 0 {
		return
	}
	recomputedBase := serverid.GenerateContextKey(serverid.ModeIsolated, cmd, args, nil, cwd)
	ctrI, _ := d.forcedIsolatedRetryCounters.LoadOrStore(recomputedBase, &atomic.Int64{})
	ctr := ctrI.(*atomic.Int64)
	for {
		cur := ctr.Load()
		if cur >= n {
			return
		}
		if ctr.CompareAndSwap(cur, n) {
			d.logger.Printf("snapshot: rehydrated forced-isolated retry counter base=%s counter=%d from sid=%s",
				shortServerID(recomputedBase), n, shortServerID(sid))
			return
		}
	}
}

const restartMaterializationBarrierTimeout = 15 * time.Second

// DaemonSnapshot is an alias for mcpsnapshot.DaemonSnapshot.
// Re-exported here so daemon-internal code can reference it without
// the package qualifier while tests continue to use the type directly.
type DaemonSnapshot = mcpsnapshot.DaemonSnapshot

// SnapshotPath returns the well-known path for the daemon state snapshot file.
func SnapshotPath() string {
	return mcpsnapshot.SnapshotPath("")
}

// snapshotOwnerPin is an immutable daemon-side view paired with an owner
// lifetime lease. Owner-local state is exported only after d.mu is released.
type snapshotOwnerPin struct {
	entry                       *OwnerEntry
	owner                       *owner.Owner
	serverID                    string
	mode                        string
	env                         map[string]string
	persistent                  bool
	ownerGeneration             string
	restoredFromOwnerGeneration string
	restoreSource               string
}

type snapshotRestartLease struct {
	daemon       *Daemon
	snapshot     *DaemonSnapshot
	registryOnce sync.Once
	ownerOnce    sync.Once
	registryPins []snapshotOwnerPin
	ownerPins    []*owner.RestartPin
}

func (l *snapshotRestartLease) ReleaseRegistryPins() {
	if l == nil || l.daemon == nil {
		return
	}
	l.registryOnce.Do(func() { l.daemon.releaseSnapshotOwnerPins(l.registryPins) })
}

func (l *snapshotRestartLease) ReleaseOwnerPins() {
	if l == nil {
		return
	}
	l.ownerOnce.Do(func() {
		for _, pin := range l.ownerPins {
			pin.Release()
		}
	})
}

func (l *snapshotRestartLease) Release() {
	if l == nil {
		return
	}
	l.ReleaseRegistryPins()
	l.ReleaseOwnerPins()
}

func (l *snapshotRestartLease) RewriteSnapshot() (string, error) {
	if l == nil || l.daemon == nil || l.snapshot == nil {
		return "", errors.New("restart snapshot lease has no retained payload")
	}
	return mcpsnapshot.Serialize(l.snapshot, l.daemon.logger)
}

// SerializeSnapshot walks all owners, exports their state, and delegates
// to mcpsnapshot.Serialize for the atomic write. Standalone callers release
// restart pins after the snapshot is durable; graceful restart uses the
// pinned variant below and retains the lease through predecessor shutdown.
func (d *Daemon) SerializeSnapshot() (string, error) {
	path, lease, err := d.serializeSnapshotPinned()
	lease.Release()
	return path, err
}

func (d *Daemon) serializeSnapshotPinned() (string, *snapshotRestartLease, error) {
	registryPins, err := d.acquireSnapshotOwnerPins()
	if err != nil {
		return "", nil, err
	}
	lease := &snapshotRestartLease{daemon: d, registryPins: registryPins}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			lease.Release()
		}
	}()

	barrierCtx, cancelBarrier := context.WithTimeout(context.Background(), restartMaterializationBarrierTimeout)
	defer cancelBarrier()
	lease.ownerPins = make([]*owner.RestartPin, 0, len(registryPins))
	for _, pin := range registryPins {
		if d.beforeSnapshotOwnerExport != nil {
			d.beforeSnapshotOwnerExport()
		}
		ownerPin, err := pin.owner.AcquireRestartPin(barrierCtx)
		if err != nil {
			return "", nil, fmt.Errorf("serialize owner %s materialization: %w", pin.serverID, err)
		}
		lease.ownerPins = append(lease.ownerPins, ownerPin)
	}

	owners := make([]mcpsnapshot.OwnerSnapshot, 0, len(registryPins))
	sessions := make([]mcpsnapshot.SessionSnapshot, 0)
	for i, pin := range registryPins {
		snap, ownerSessions := lease.ownerPins[i].Snapshot, lease.ownerPins[i].Sessions
		snap.ServerID = pin.serverID
		snap.Mode = pin.mode
		if snap.Env == nil {
			snap.Env = cloneSnapshotStringMap(pin.env)
		}
		snap.Persistent = pin.persistent || snap.Persistent
		snap.OwnerGeneration = pin.ownerGeneration
		snap.RestoredFromGeneration = pin.restoredFromOwnerGeneration
		snap.RestoreSource = pin.restoreSource
		owners = append(owners, snap)

		for _, ss := range ownerSessions {
			ss.OwnerServerID = pin.serverID
			sessions = append(sessions, ss)
		}
	}

	data := &DaemonSnapshot{
		Version:          mcpsnapshot.SnapshotVersion,
		MuxVersion:       owner.Version,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		DaemonGeneration: d.daemonGeneration,
		PredecessorPID:   os.Getpid(),
		Owners:           owners,
		Sessions:         sessions,
	}
	lease.snapshot = data

	path, err := mcpsnapshot.Serialize(data, d.logger)
	if err != nil {
		return "", nil, err
	}
	releaseOnError = false
	return path, lease, nil
}

func (d *Daemon) acquireSnapshotOwnerPins() ([]snapshotOwnerPin, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for sid, entry := range d.owners {
		if entry != nil && entry.Owner != nil && entry.removalInProgress {
			return nil, fmt.Errorf("snapshot owner %s removal is still finalizing", shortServerID(sid))
		}
	}
	pins := make([]snapshotOwnerPin, 0, len(d.owners))
	for sid, entry := range d.owners {
		if entry.Owner == nil {
			continue
		}
		if entry.snapshotPins == 0 {
			entry.snapshotUnpinned = make(chan struct{})
		}
		entry.snapshotPins++
		pins = append(pins, snapshotOwnerPin{
			entry:                       entry,
			owner:                       entry.Owner,
			serverID:                    sid,
			mode:                        entry.Mode,
			env:                         cloneSnapshotStringMap(entry.Env),
			persistent:                  entry.Persistent,
			ownerGeneration:             entry.OwnerGeneration,
			restoredFromOwnerGeneration: entry.RestoredFromOwnerGeneration,
			restoreSource:               entry.RestoreSource,
		})
	}
	return pins, nil
}

func (d *Daemon) releaseSnapshotOwnerPins(pins []snapshotOwnerPin) {
	d.mu.Lock()
	for _, pin := range pins {
		entry := pin.entry
		if entry.snapshotPins <= 0 {
			continue
		}
		entry.snapshotPins--
		if entry.snapshotPins == 0 {
			close(entry.snapshotUnpinned)
			entry.snapshotUnpinned = nil
		}
	}
	d.mu.Unlock()
}

func cloneSnapshotStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneOwnerSnapshotForRecovery(src mcpsnapshot.OwnerSnapshot) mcpsnapshot.OwnerSnapshot {
	dst := src
	dst.Args = append([]string(nil), src.Args...)
	dst.Env = cloneSnapshotStringMap(src.Env)
	dst.CwdSet = append([]string(nil), src.CwdSet...)
	dst.ClassificationReason = append([]string(nil), src.ClassificationReason...)
	dst.BoundTokens = append([]mcpsnapshot.BoundTokenSnapshot(nil), src.BoundTokens...)
	for i := range dst.BoundTokens {
		dst.BoundTokens[i].Env = cloneSnapshotStringMap(src.BoundTokens[i].Env)
	}
	return dst
}

func cloneSessionSnapshotForRecovery(src mcpsnapshot.SessionSnapshot) mcpsnapshot.SessionSnapshot {
	dst := src
	dst.Env = cloneSnapshotStringMap(src.Env)
	return dst
}

func buildRestartRecoverySnapshot(base *DaemonSnapshot, plans []snapshotRestorePlan) *DaemonSnapshot {
	if base == nil || len(plans) == 0 {
		return nil
	}
	recovery := *base
	recovery.Owners = make([]mcpsnapshot.OwnerSnapshot, 0, len(plans))
	selected := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		sid := plan.snapshot.ServerID
		if _, duplicate := selected[sid]; duplicate {
			continue
		}
		selected[sid] = struct{}{}
		recovery.Owners = append(recovery.Owners, cloneOwnerSnapshotForRecovery(plan.snapshot))
	}
	recovery.Sessions = make([]mcpsnapshot.SessionSnapshot, 0, len(base.Sessions))
	for _, session := range base.Sessions {
		if _, keep := selected[session.OwnerServerID]; keep {
			recovery.Sessions = append(recovery.Sessions, cloneSessionSnapshotForRecovery(session))
		}
	}
	return &recovery
}

func (d *Daemon) rewriteRestartRecoverySnapshot() (string, error) {
	d.mu.RLock()
	retained := d.restartRecoverySnapshot
	if retained == nil {
		d.mu.RUnlock()
		return "", errors.New("restart recovery snapshot is unavailable")
	}
	recovery := *retained
	recovery.Owners = make([]mcpsnapshot.OwnerSnapshot, len(retained.Owners))
	for i, ownerSnap := range retained.Owners {
		recovery.Owners[i] = cloneOwnerSnapshotForRecovery(ownerSnap)
	}
	recovery.Sessions = make([]mcpsnapshot.SessionSnapshot, len(retained.Sessions))
	for i, session := range retained.Sessions {
		recovery.Sessions[i] = cloneSessionSnapshotForRecovery(session)
	}
	d.mu.RUnlock()

	recovery.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	recovery.DaemonGeneration = d.daemonGeneration
	recovery.PredecessorPID = os.Getpid()
	return mcpsnapshot.Serialize(&recovery, d.logger)
}

// DeserializeSnapshot reads and validates a snapshot from the well-known path.
// Returns nil, nil if no snapshot exists (cold start). Deletes the file after
// successful load or if stale. Logs warnings for corrupt/stale snapshots.
func DeserializeSnapshot(logger interface{ Printf(string, ...any) }) (*DaemonSnapshot, error) {
	return mcpsnapshot.Deserialize(logger)
}

// dialHandoffHook is overridable by tests to inject a mock fdConn instead of
// dialing a real socket. Production code always leaves this nil.
var dialHandoffHook func(socketPath string, timeout time.Duration) (fdConn, error)

// tryHandoffReceive checks for MCPMUX_HANDOFF_TOKEN_PATH and MCPMUX_HANDOFF_SOCKET
// env vars. If both are set, dials the handoff socket, authenticates with the token,
// and receives the list of upstream FDs from the old daemon.
// Returns nil on any failure (FR-8 fallback: caller uses SpawnUpstreamBackground for all owners).
func (d *Daemon) tryHandoffReceive(ctx context.Context) *handoffReceipt {
	tokenPath := os.Getenv("MCPMUX_HANDOFF_TOKEN_PATH")
	socketPath := os.Getenv("MCPMUX_HANDOFF_SOCKET")

	if tokenPath == "" || socketPath == "" {
		return nil
	}
	defer deleteHandoffToken(tokenPath) //nolint:errcheck

	token, err := readHandoffToken(tokenPath)
	if err != nil {
		d.logger.Printf("handoff.receive.fail reason=%v", err)
		return nil
	}

	dialFn := dialHandoff
	if dialHandoffHook != nil {
		dialFn = dialHandoffHook
	}
	conn, err := dialFn(socketPath, 2*time.Second)
	if err != nil {
		d.logger.Printf("handoff.receive.fail reason=%v", err)
		return nil
	}

	receiveCtx, cancel := context.WithTimeout(ctx, handoffTotalTimeout)
	defer cancel()

	receipt, err := prepareHandoffReceive(receiveCtx, conn, token)
	if err != nil {
		_ = conn.Close()
		d.logger.Printf("handoff.receive.fail reason=%v", err)
		return nil
	}
	d.logger.Printf("handoff.receive.prepared upstreams=%d", len(receipt.received))
	return receipt
}

type snapshotRestorePlan struct {
	snapshot  mcpsnapshot.OwnerSnapshot
	cfg       owner.OwnerConfig
	env       map[string]string
	blockedBy *OwnerEntry
}

func (d *Daemon) makeSnapshotRestorePlan(ownerSnap mcpsnapshot.OwnerSnapshot) snapshotRestorePlan {
	restoredEnv := cloneSnapshotStringMap(ownerSnap.Env)
	command := ownerSnap.Command
	args := append([]string(nil), ownerSnap.Args...)
	cfg := owner.OwnerConfig{
		Command:               command,
		Args:                  args,
		Env:                   restoredEnv,
		Cwd:                   ownerSnap.Cwd,
		IPCPath:               serverid.IPCPath("", d.namespace, ownerSnap.ServerID),
		ControlPath:           serverid.ControlPath("", d.namespace, ownerSnap.ServerID),
		ServerID:              ownerSnap.ServerID,
		TokenHandshake:        true,
		MaterializationPolicy: owner.MaterializationOnDemand,
		PersistentPending:     ownerSnap.Persistent,
		PersistentRequired:    d.persistent,
		HandlerFunc:           d.handlerFunc,
		SessionHandler:        d.sessionHandler,
		AuthorizeSession:      d.authorizeSession,
		OnFrameReceived:       d.onFrameReceived,
		OnZeroSessions:        d.onZeroSessions,
		OnUpstreamExit:        d.onUpstreamExit,
		OnPersistentDetected: func(expected *owner.Owner) {
			d.setOwnerPersistent(expected, true)
		},
		OnPersistentResolved: d.resolveOwnerPersistent,
		OnCacheReady:         d.publishOwnerCache,
		OnCacheInvalidated:   d.invalidateOwnerTemplate,
		Logger:               log.New(d.logger.Writer(), fmt.Sprintf("[mcp-mux:%s] ", shortServerID(ownerSnap.ServerID)), log.LstdFlags|log.Lmicroseconds),
	}
	if d.persistent || ownerSnap.Persistent {
		cfg.MaterializationPolicy = owner.MaterializationPersistent
	}
	return snapshotRestorePlan{snapshot: ownerSnap, cfg: cfg, env: restoredEnv}
}

func (d *Daemon) restoreSnapshotPlan(plan snapshotRestorePlan, handoff *HandoffUpstream, restoreSource string, eager, publishTemplate bool) (*OwnerEntry, bool, error) {
	snap := plan.snapshot
	if isRestartRestoreMode() {
		// Snapshot capture cannot exclude a late list_changed or discovery
		// response from the predecessor. Preserve initialize for protocol and
		// runtime policy, but force every secondary discovery list through the
		// adopted live generation before replay or template publication.
		snap.CachedTools = ""
		snap.CachedPrompts = ""
		snap.CachedResources = ""
		snap.CachedResourceTemplates = ""
	}
	if isRestartRestoreMode() {
		d.retireOldOwnerSockets(plan.cfg.IPCPath, plan.cfg.ControlPath)
	}

	var restoredOwner *owner.Owner
	reattached := false
	var err error
	if handoff != nil {
		plan.cfg.CachedClassification = snap.Classification
		plan.cfg.AdoptedSnapshot = &snap
		payload := owner.HandoffPayload{
			ServerID:    snap.ServerID,
			PID:         handoff.PID,
			StdinFD:     handoff.StdinFD,
			StdoutFD:    handoff.StdoutFD,
			StderrFD:    handoff.StderrFD,
			AuthorityFD: handoff.AuthorityFD,
			Command:     handoff.Command,
			Args:        snap.Args,
			Cwd:         snap.Cwd,
		}
		restoredOwner, err = owner.NewOwnerFromHandoff(plan.cfg, payload)
		reattached = err == nil
	} else {
		restoredOwner, err = owner.NewOwnerFromSnapshot(plan.cfg, snap)
	}
	if err != nil {
		return nil, false, err
	}

	ownerGeneration, err := generateGeneration("owner")
	if err != nil {
		restoredOwner.Shutdown()
		d.logger.Printf("snapshot: failed to generate owner generation for %s: %v", shortServerID(snap.ServerID), err)
		return nil, false, err
	}
	var serviceToken suture.ServiceToken
	if d.supervisor != nil {
		serviceToken = d.supervisor.Add(restoredOwner)
	}
	effectivePersistent := d.persistent || snap.Persistent
	entry := &OwnerEntry{
		Owner:                       restoredOwner,
		ServerID:                    snap.ServerID,
		Command:                     snap.Command,
		Args:                        append([]string(nil), snap.Args...),
		Cwd:                         snap.Cwd,
		Mode:                        snap.Mode,
		Env:                         plan.env,
		Persistent:                  effectivePersistent,
		LastSession:                 time.Now(),
		OwnerGeneration:             ownerGeneration,
		RestoredFromOwnerGeneration: snap.OwnerGeneration,
		RestoreSource:               restoreSource,
		IdleTimeout:                 d.ownerIdleTimeout,
		serviceToken:                serviceToken,
	}
	d.mu.Lock()
	if _, exists := d.owners[snap.ServerID]; exists {
		d.mu.Unlock()
		if d.supervisor != nil {
			d.supervisor.Remove(serviceToken)
		}
		restoredOwner.Shutdown()
		return nil, false, fmt.Errorf("snapshot owner %s already registered", shortServerID(snap.ServerID))
	}
	d.owners[snap.ServerID] = entry
	d.mu.Unlock()
	restoredOwner.ResolvePersistent(effectivePersistent)
	d.rehydrateRetryCounter(snap.ServerID, snap.Command, snap.Args, snap.Cwd)
	if publishTemplate && snap.CachedInit != "" && snap.CachedTools != "" {
		snap.Persistent = effectivePersistent
		if !d.publishOwnerCache(restoredOwner, snap) {
			d.logger.Printf("snapshot: cache publish rejected for stale owner %s", shortServerID(snap.ServerID))
		}
	}
	if eager && !reattached {
		restoredOwner.SpawnUpstreamBackground()
	}
	if reattached {
		d.logger.Printf("snapshot: reattached owner %s from handoff (pid=%d)", shortServerID(snap.ServerID), handoff.PID)
	} else {
		d.logger.Printf("snapshot: restored owner %s for %s %v source=%s eager=%v", shortServerID(snap.ServerID), snap.Command, snap.Args, restoreSource, eager)
	}
	return entry, reattached, nil
}

func (d *Daemon) activateRestartStaging() (int, error) {
	d.mu.RLock()
	plans := append([]snapshotRestorePlan(nil), d.restartStaging...)
	d.mu.RUnlock()
	if err := d.waitForRestartStagingBlockers(plans); err != nil {
		return 0, err
	}

	type activatedOwner struct {
		plan  snapshotRestorePlan
		entry *OwnerEntry
	}
	activated := make([]activatedOwner, 0, len(plans))
	for _, plan := range plans {
		entry, _, err := d.restoreSnapshotPlan(plan, nil, "snapshot_fallback", false, true)
		if err == nil {
			activated = append(activated, activatedOwner{plan: plan, entry: entry})
			continue
		}

		activationErr := fmt.Errorf("snapshot: staged restore failed for %s (%s): %w", shortServerID(plan.snapshot.ServerID), plan.snapshot.Command, err)
		rollbackErrs := make([]error, 0, len(activated))
		for i := len(activated) - 1; i >= 0; i-- {
			item := activated[i]
			result, removeErr := d.removeOwnerIfCurrent(item.plan.snapshot.ServerID, item.entry, ownerRemovalReasonRestoreFailed, false)
			if removeErr != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("rollback staged owner %s: %w", shortServerID(item.plan.snapshot.ServerID), removeErr))
			} else if !result.Removed {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("rollback staged owner %s: owner remained registered", shortServerID(item.plan.snapshot.ServerID)))
			}
		}
		if len(rollbackErrs) > 0 {
			return 0, errors.Join(append([]error{activationErr}, rollbackErrs...)...)
		}
		return 0, activationErr
	}

	d.mu.Lock()
	d.restartStaging = nil
	d.restartRecoverySnapshot = nil
	d.mu.Unlock()
	// Construct and register the complete staged set before any fallback process
	// starts. A later registration failure can then roll back inert owners without
	// creating a transient extra process generation.
	for _, item := range activated {
		item.entry.Owner.SpawnUpstreamBackground()
	}
	restored := len(activated)
	if restored > 0 {
		d.restoredOwnerCount.Add(uint64(restored))
		d.runRestoreHealthGate()
	}
	d.logger.Printf("snapshot: activated %d/%d staged owners after predecessor finalization", restored, len(plans))
	return restored, nil
}

func (d *Daemon) waitForRestartStagingBlockers(plans []snapshotRestorePlan) error {
	deadline := time.NewTimer(restartMaterializationBarrierTimeout)
	defer deadline.Stop()
	for _, plan := range plans {
		if plan.blockedBy == nil {
			continue
		}
		for {
			d.mu.RLock()
			current := d.owners[plan.snapshot.ServerID]
			d.mu.RUnlock()
			if current == nil {
				break
			}
			if current != plan.blockedBy {
				return fmt.Errorf("restart fallback owner %s was replaced before rollback finalization", shortServerID(plan.snapshot.ServerID))
			}
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-timer.C:
			case <-deadline.C:
				timer.Stop()
				return fmt.Errorf("restart fallback owner %s finalization remained unproven after %s", shortServerID(plan.snapshot.ServerID), restartMaterializationBarrierTimeout)
			case <-d.done:
				timer.Stop()
				return errors.New("daemon stopped while waiting for restart rollback finalization")
			}
		}
	}
	return nil
}

// loadSnapshot checks for a snapshot file and restores owners from it.
// Called on daemon startup. If no snapshot exists, returns 0 (cold start).
// Returns the number of owners restored.
func (d *Daemon) loadSnapshot() int {
	snap, err := DeserializeSnapshot(d.logger)
	if err != nil {
		d.logger.Printf("snapshot load error: %v", err)
		return 0
	}
	if snap == nil {
		return 0
	}

	d.mu.Lock()
	d.predecessorPID = snap.PredecessorPID
	d.predecessorDaemonGeneration = snap.DaemonGeneration
	d.mu.Unlock()

	restartMode := isRestartRestoreMode()
	handoffReceipt := d.tryHandoffReceive(context.Background())
	handoffAccepted := make([]string, 0)
	type adoptedHandoffOwner struct {
		plan  snapshotRestorePlan
		entry *OwnerEntry
	}
	adopted := make([]adoptedHandoffOwner, 0)
	staged := make([]snapshotRestorePlan, 0)
	restored := 0

	for _, ownerSnap := range snap.Owners {
		if ownerSnap.Classification == classify.ModeIsolated && len(ownerSnap.CwdSet) > 1 {
			d.logger.Printf("snapshot: healing poisoned isolated owner %s: cwdSet %v -> [%s]",
				shortServerID(ownerSnap.ServerID), ownerSnap.CwdSet, ownerSnap.Cwd)
			ownerSnap.CwdSet = []string{ownerSnap.Cwd}
		}
		plan := d.makeSnapshotRestorePlan(ownerSnap)

		if restartMode {
			if handoffReceipt != nil {
				if transferred, ok := handoffReceipt.take(ownerSnap.ServerID); ok {
					entry, _, restoreErr := d.restoreSnapshotPlan(plan, &transferred, "snapshot_handoff", false, false)
					if restoreErr == nil {
						handoffAccepted = append(handoffAccepted, ownerSnap.ServerID)
						adopted = append(adopted, adoptedHandoffOwner{plan: plan, entry: entry})
						restored++
						continue
					}
					d.logger.Printf("snapshot: handoff reattach failed for %s: %v; staging eager fallback",
						shortServerID(ownerSnap.ServerID), restoreErr)
				}
			}
			// No transferred process authority: keep metadata successor-local.
			// IPC binding, supervisor registration, and process start wait until
			// final ACK plus control-socket takeover prove predecessor finalization.
			staged = append(staged, plan)
			continue
		}

		if _, _, restoreErr := d.restoreSnapshotPlan(plan, nil, "snapshot_fallback", true, true); restoreErr != nil {
			d.logger.Printf("snapshot: failed to restore owner %s (%s): %v", shortServerID(ownerSnap.ServerID), ownerSnap.Command, restoreErr)
			continue
		}
		restored++
	}

	if handoffReceipt != nil {
		if finalizeErr := handoffReceipt.finalize(handoffAccepted); finalizeErr != nil {
			d.logger.Printf("handoff.receive.commit_fail accepted=%d reason=%v", len(handoffAccepted), finalizeErr)
			fallback := make([]snapshotRestorePlan, 0, len(adopted))
			for _, item := range adopted {
				sid := item.plan.snapshot.ServerID
				result, removeErr := d.removeOwnerIfCurrent(sid, item.entry, ownerRemovalReasonRestoreFailed, false)
				if removeErr != nil {
					d.logger.Printf("handoff.receive.rollback_remove_fail server_id=%s reason=%v", shortServerID(sid), removeErr)
				}
				if !result.Removed {
					item.entry.Owner.Shutdown()
					item.plan.blockedBy = item.entry
				}
				fallback = append(fallback, item.plan)
			}
			// A failed global final ACK discards cache-only/no-handle staging.
			// Only previously detached owners receive one post-barrier fallback.
			d.logger.Printf("handoff.receive.discard_staging count=%d", len(staged))
			staged = fallback
			restored = 0
		} else {
			// Adopted owners intentionally begin with secondary discovery caches
			// invalidated. Their first live tools/list response republishes a fresh
			// template through publishOwnerCache.
			d.logger.Printf("handoff.receive.ok upstreams=%d staged=%d", len(handoffAccepted), len(staged))
		}
	}

	if restartMode {
		recoveryPlans := append([]snapshotRestorePlan(nil), staged...)
		if restored > 0 {
			for _, item := range adopted {
				recoveryPlans = append(recoveryPlans, item.plan)
			}
		}
		d.mu.Lock()
		d.restartStaging = append([]snapshotRestorePlan(nil), staged...)
		d.restartRecoverySnapshot = buildRestartRecoverySnapshot(snap, recoveryPlans)
		d.mu.Unlock()
		if restored > 0 {
			d.restoredOwnerCount.Add(uint64(restored))
		}
		planned := restored + len(staged)
		d.logger.Printf("snapshot: restored %d process-backed owners; staged %d/%d metadata owners", restored, len(staged), len(snap.Owners))
		return planned
	}

	if restored > 0 {
		d.restoredOwnerCount.Add(uint64(restored))
		d.runRestoreHealthGate()
	}
	d.logger.Printf("snapshot: restored %d/%d owners", restored, len(snap.Owners))
	return restored
}

func (d *Daemon) loadSnapshotMetadataOnly(reason string) int {
	snap, err := DeserializeSnapshot(d.logger)
	if err != nil {
		d.logger.Printf("snapshot load error: %v", err)
		return 0
	}
	if snap == nil {
		return 0
	}

	d.mu.Lock()
	d.predecessorPID = snap.PredecessorPID
	d.predecessorDaemonGeneration = snap.DaemonGeneration
	d.mu.Unlock()

	restoreCaches := !isRestartRestoreMode()
	for _, ownerSnap := range snap.Owners {
		if restoreCaches && ownerSnap.CachedInit != "" && ownerSnap.CachedTools != "" {
			d.updateTemplate(ownerSnap.Command, ownerSnap.Args, ownerSnap)
		}
		d.rehydrateRetryCounter(ownerSnap.ServerID, ownerSnap.Command, ownerSnap.Args, ownerSnap.Cwd)
	}

	d.logger.Printf("snapshot: deferred restore of %d owners (%s)", len(snap.Owners), reason)
	return len(snap.Owners)
}

// restoreHealthGateWindow is the time we allow newly-restored owners to fully
// bind their IPC listeners before the FR-3 sweep runs. Calibrated above the
// ipc.Dial 500ms timeout plus a small margin for scheduler jitter on slow CI
// runners. Declared as var so tests can override it.
var restoreHealthGateWindow = 750 * time.Millisecond

// runRestoreHealthGate walks every owner currently in d.owners and verifies
// its listener is reachable via an outbound dial probe. Entries that fail the
// probe are torn down and removed from the registry.
//
// Runs in a goroutine so the probe sweep does not block the startup path;
// the goroutine logs its summary and exits. Each probe uses ipc.Dial's
// 500ms timeout. The sweep takes an RLock snapshot of the owners map, then
// re-acquires the write lock under CAS (entry still matches what we probed)
// for each zombie found, so concurrent spawn/shutdown cannot produce torn
// state.
func (d *Daemon) runRestoreHealthGate() {
	// Read the tunable once here (main goroutine) and pass into the worker
	// via closure capture. Reading it inside the worker would race t.Cleanup
	// callers in tests that restore the var after the test function returns
	// but before the goroutine wakes.
	window := restoreHealthGateWindow
	go func() {
		// Respect daemon shutdown: if Shutdown closes d.done during our
		// sleep window, exit immediately instead of sweeping a daemon that
		// is already tearing itself down.
		select {
		case <-time.After(window):
		case <-d.done:
			return
		}

		d.mu.RLock()
		entries := make([]*OwnerEntry, 0, len(d.owners))
		for _, e := range d.owners {
			if e.Owner != nil {
				entries = append(entries, e)
			}
		}
		d.mu.RUnlock()

		zombies := 0
		for _, entry := range entries {
			// Re-check shutdown between probes so a large owner set cannot
			// extend our presence on a dying daemon.
			select {
			case <-d.done:
				return
			default:
			}

			// IMPORTANT: a zombie is an owner whose listener died WITHOUT a
			// closeListener() call — i.e. IsAccepting reports true (sync
			// channel still open) but IsReachable reports false (dial fails).
			// Owners that legitimately closed their listener (e.g. isolated
			// servers after the first session connects) report IsAccepting
			// false and IsReachable false; they are NOT zombies and we MUST
			// NOT tear them down here — the health gate is a defensive
			// check against the unreachable-despite-IsAccepting class only.
			if !entry.Owner.IsAccepting() {
				continue
			}
			// Probe outside d.mu — ipc.Dial can take up to 500ms.
			if entry.Owner.IsReachable() {
				continue
			}

			d.mu.Lock()
			sid := entry.ServerID
			current, ok := d.owners[sid]
			if !ok || current != entry {
				d.mu.Unlock()
				continue
			}
			d.zombieDetectedRestore++
			shortSID := sid
			if len(shortSID) > 8 {
				shortSID = shortSID[:8]
			}
			d.logger.Printf(
				"zombie-listener detected: path=restore server=%s ipc=%q cmd=%q action=tear-down-and-respawn-on-demand",
				shortSID, entry.Owner.IPCPath(), entry.Command,
			)
			d.mu.Unlock()
			if _, err := d.removeOwnerIfCurrent(sid, entry, ownerRemovalReasonRestoreFailed, false); err != nil {
				d.logger.Printf("post-restore health gate: cleanup failed for %s: %v", shortSID, err)
			}
			zombies++
		}
		if zombies > 0 {
			d.logger.Printf("post-restore health gate: tore down %d zombie owner(s)", zombies)
		}
	}()
}
