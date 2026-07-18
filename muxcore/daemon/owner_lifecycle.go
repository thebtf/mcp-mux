package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thejerf/suture/v4"
)

type ownerRemovalReason string

const (
	ownerRemovalReasonOperatorHard  ownerRemovalReason = "operator_hard"
	ownerRemovalReasonOperatorSoft  ownerRemovalReason = "operator_soft"
	ownerRemovalReasonIdle          ownerRemovalReason = "idle"
	ownerRemovalReasonZombie        ownerRemovalReason = "zombie"
	ownerRemovalReasonHandoff       ownerRemovalReason = "handoff"
	ownerRemovalReasonRestoreFailed ownerRemovalReason = "restore_failed"
	ownerRemovalReasonUpstreamExit  ownerRemovalReason = "upstream_exit"
)

const (
	snapshotPinWaitTimeout      = 30 * time.Second
	ownerFinalizationAttempts   = 3
	ownerFinalizationRetryDelay = 10 * time.Millisecond
)

var finalizeOwnerForRemoval = func(o *owner.Owner, soft bool) (int, bool, error) {
	return o.FinalizeForRemoval(soft, 30*time.Second)
}

type ownerRemovalResult struct {
	ServerID             string
	Reason               ownerRemovalReason
	PendingTokensRemoved int
	BoundHistoryRemoved  int
	Soft                 bool
	Removed              bool
}

type ownerRemovalStats struct {
	Total                uint64
	ByReason             map[ownerRemovalReason]uint64
	PendingTokensRemoved uint64
	BoundHistoryRemoved  uint64
}

type preparedOwnerRemoval struct {
	result          ownerRemovalResult
	token           suture.ServiceToken
	exitCode        int
	finalizationErr error
}

func newOwnerRemovalStats() ownerRemovalStats {
	return ownerRemovalStats{ByReason: make(map[ownerRemovalReason]uint64)}
}

func (s ownerRemovalStats) statusMap() map[string]any {
	byReason := make(map[string]uint64, len(s.ByReason))
	for reason, count := range s.ByReason {
		byReason[string(reason)] = count
	}
	return map[string]any{
		"total":                  s.Total,
		"by_reason":              byReason,
		"pending_tokens_removed": s.PendingTokensRemoved,
		"bound_history_removed":  s.BoundHistoryRemoved,
	}
}

func (d *Daemon) removeOwner(serverID string, reason ownerRemovalReason, soft bool) (ownerRemovalResult, error) {
	return d.removeOwnerIfCurrent(serverID, nil, reason, soft)
}

func (d *Daemon) removeOwnerIfCurrent(serverID string, expected *OwnerEntry, reason ownerRemovalReason, soft bool) (ownerRemovalResult, error) {
	return d.finalizeAndRemoveOwner(serverID, expected, reason, soft, nil, true)
}

func (d *Daemon) removeOwnerIfCurrentAndZeroIdle(serverID string, expected *OwnerEntry, zeroAt time.Time, idleTimeout time.Duration) (ownerRemovalResult, bool, error) {
	result := ownerRemovalResult{ServerID: serverID, Reason: ownerRemovalReasonIdle, Soft: true}
	d.mu.RLock()
	entry, ok := d.owners[serverID]
	if !ok || (expected != nil && entry != expected) || entry == nil || entry.Owner == nil || !entry.LastSession.Equal(zeroAt) {
		d.mu.RUnlock()
		return result, false, nil
	}
	ownerRef := entry.Owner
	persistent := entry.Persistent
	lastSession := entry.LastSession
	d.mu.RUnlock()

	sample := evictionSample{
		Sessions:               ownerRef.SessionCount(),
		PendingSessions:        ownerRef.SessionMgr().PendingCount(),
		Persistent:             persistent,
		PendingRequests:        ownerRef.PendingRequests(),
		ActiveProgressTokens:   ownerRef.ActiveProgressTokens(),
		HasBusyWork:            ownerRef.HasActiveBusyWork(),
		UpstreamDead:           ownerRef.UpstreamDead(),
		CacheReady:             ownerRef.CacheReady(),
		MaterializationBlocked: ownerRef.MaterializationBlocksEviction(),
		IdleTimeout:            idleTimeout,
		OwnerIdleOverride:      ownerRef.IdleTimeout(),
		LastSession:            lastSession,
		LastActivity:           ownerRef.LastActivity(),
		IsolatedClassified:     ownerRef.IsClassifiedIsolated(),
	}
	decision := shouldEvict(sample, time.Now(), idleTimeout, 0)
	if !decision.evict {
		return result, false, nil
	}

	reason := ownerRemovalReasonIdle
	soft := true
	if decision.reason == "zombie" {
		reason = ownerRemovalReasonZombie
		soft = false
	}
	removed, err := d.finalizeAndRemoveOwner(serverID, entry, reason, soft, func(current *OwnerEntry) bool {
		return current.LastSession.Equal(zeroAt)
	}, true)
	return removed, removed.Removed, err
}

func (d *Daemon) finalizeAndRemoveOwner(serverID string, expected *OwnerEntry, reason ownerRemovalReason, soft bool, eligible func(*OwnerEntry) bool, scheduleRetry bool) (ownerRemovalResult, error) {
	result := ownerRemovalResult{ServerID: serverID, Reason: reason, Soft: soft}
	var entry *OwnerEntry
	for {
		d.mu.Lock()
		current, ok := d.owners[serverID]
		if !ok {
			d.mu.Unlock()
			if expected == nil {
				return result, fmt.Errorf("server %s not found", serverID)
			}
			return result, nil
		}
		if expected != nil && current != expected {
			d.mu.Unlock()
			return result, nil
		}
		if current.Owner == nil {
			d.mu.Unlock()
			return result, fmt.Errorf("server %s is still being created", serverID)
		}
		if eligible != nil && !eligible(current) {
			d.mu.Unlock()
			return result, nil
		}
		if current.removalInProgress {
			settled := current.removalDone
			d.mu.Unlock()
			if err := waitForOwnerRemovalAttempt(serverID, settled); err != nil {
				return result, err
			}
			continue
		}
		if current.snapshotPins > 0 {
			unpinned := current.snapshotUnpinned
			d.mu.Unlock()
			if err := waitForSnapshotUnpin(serverID, unpinned); err != nil {
				return result, err
			}
			continue
		}
		current.removalInProgress = true
		current.removalDone = make(chan struct{})
		current.terminationHint = terminationHintForRemoval(reason)
		entry = current
		d.mu.Unlock()
		break
	}

	exitCode, finalized, finalizationErr := d.finalizeOwnerWithRetry(entry.Owner, soft)
	if !finalized {
		d.mu.Lock()
		finishOwnerRemovalAttemptLocked(entry)
		d.mu.Unlock()
		if scheduleRetry {
			d.scheduleOwnerFinalizationRetry(serverID, entry, reason, soft, eligible)
		}
		return result, finalizationErr
	}

	d.mu.Lock()
	current, ok := d.owners[serverID]
	if !ok || current != entry {
		finishOwnerRemovalAttemptLocked(entry)
		d.mu.Unlock()
		return result, finalizationErr
	}
	if entry.snapshotPins > 0 {
		finishOwnerRemovalAttemptLocked(entry)
		d.mu.Unlock()
		pinErr := fmt.Errorf("owner %s gained a snapshot pin during finalization", shortServerID(serverID))
		if scheduleRetry {
			d.scheduleOwnerFinalizationRetry(serverID, entry, reason, soft, eligible)
		}
		return result, errors.Join(finalizationErr, pinErr)
	}
	finishOwnerRemovalAttemptLocked(entry)
	prepared := d.prepareOwnerRemovalLocked(serverID, entry, reason, soft)
	prepared.exitCode = exitCode
	prepared.finalizationErr = finalizationErr
	d.mu.Unlock()
	return prepared.result, errors.Join(finalizationErr, d.finishOwnerRemoval(prepared))
}

func finishOwnerRemovalAttemptLocked(entry *OwnerEntry) {
	if entry == nil || !entry.removalInProgress {
		return
	}
	entry.removalInProgress = false
	if entry.removalDone != nil {
		close(entry.removalDone)
		entry.removalDone = nil
	}
}

func waitForOwnerRemovalAttempt(serverID string, settled <-chan struct{}) error {
	if settled == nil {
		return fmt.Errorf("server %s removal attempt missing completion signal", serverID)
	}
	timer := time.NewTimer(snapshotPinWaitTimeout)
	defer timer.Stop()
	select {
	case <-settled:
		return nil
	case <-timer.C:
		return fmt.Errorf("server %s removal attempt did not settle within %s", serverID, snapshotPinWaitTimeout)
	}
}

func (d *Daemon) finalizeOwnerWithRetry(ownerRef *owner.Owner, soft bool) (int, bool, error) {
	exitCode := 0
	var lastErr error
	for attempt := 1; attempt <= ownerFinalizationAttempts; attempt++ {
		var finalized bool
		exitCode, finalized, lastErr = finalizeOwnerForRemoval(ownerRef, soft)
		if finalized {
			return exitCode, true, lastErr
		}
		if lastErr == nil {
			lastErr = errors.New("whole-tree finalization remains unproven")
		}
		if attempt < ownerFinalizationAttempts {
			time.Sleep(ownerFinalizationRetryDelay)
		}
	}
	return exitCode, false, fmt.Errorf("owner finalization unproven after %d attempts: %w", ownerFinalizationAttempts, lastErr)
}

func (d *Daemon) scheduleOwnerFinalizationRetry(serverID string, entry *OwnerEntry, reason ownerRemovalReason, soft bool, eligible func(*OwnerEntry) bool) {
	d.mu.Lock()
	if d.owners[serverID] != entry || entry.removalRetrying {
		d.mu.Unlock()
		return
	}
	entry.removalRetrying = true
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			if d.owners[serverID] == entry {
				entry.removalRetrying = false
			}
			d.mu.Unlock()
		}()
		for {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-timer.C:
			case <-d.done:
				timer.Stop()
				return
			}
			result, err := d.finalizeAndRemoveOwner(serverID, entry, reason, soft, eligible, false)
			if result.Removed || err == nil {
				return
			}
			d.logger.Printf("owner %s: retryable finalization still blocked: %v", shortServerID(serverID), err)
		}
	}()
}

func waitForSnapshotUnpin(serverID string, unpinned <-chan struct{}) error {
	if unpinned == nil {
		return fmt.Errorf("server %s snapshot pin missing release signal", serverID)
	}
	timer := time.NewTimer(snapshotPinWaitTimeout)
	defer timer.Stop()
	select {
	case <-unpinned:
		return nil
	case <-timer.C:
		return fmt.Errorf("server %s snapshot pin did not release within %s", serverID, snapshotPinWaitTimeout)
	}
}

func (d *Daemon) prepareOwnerRemovalLocked(serverID string, entry *OwnerEntry, reason ownerRemovalReason, soft bool) preparedOwnerRemoval {
	result := ownerRemovalResult{ServerID: serverID, Reason: reason, Soft: soft}
	result.PendingTokensRemoved = entry.Owner.PendingAdmissionsPurged() + entry.Owner.SessionMgr().RemovePendingForOwner(serverID)
	result.BoundHistoryRemoved = entry.Owner.SessionMgr().RemoveBoundForOwner(serverID)
	d.recordOwnerRemovalLocked(result)
	d.deleteOwnerEntryLocked(serverID)
	result.Removed = true
	return preparedOwnerRemoval{result: result, token: entry.serviceToken}
}

func (d *Daemon) finishOwnerRemoval(prepared preparedOwnerRemoval) error {
	var supErr error
	if d.supervisor != nil {
		if err := d.supervisor.RemoveAndWait(prepared.token, 2*time.Second); err != nil {
			action := "remove"
			if prepared.result.Soft {
				action = "soft-remove"
			}
			supErr = fmt.Errorf("%s owner %s from supervisor: %w", action, shortServerID(prepared.result.ServerID), err)
			d.logger.Printf("warning: %v", supErr)
		}
	}

	if prepared.result.Soft {
		if prepared.finalizationErr != nil {
			d.logger.Printf("soft-removed owner %s after finalization warning (exit=%d err=%v)", shortServerID(prepared.result.ServerID), prepared.exitCode, prepared.finalizationErr)
		} else if prepared.exitCode == 0 {
			d.logger.Printf("soft-removed owner %s: upstream exited cleanly (code 0)", shortServerID(prepared.result.ServerID))
		} else {
			d.logger.Printf("soft-removed owner %s: upstream exited with code %d", shortServerID(prepared.result.ServerID), prepared.exitCode)
		}
	} else if prepared.finalizationErr != nil {
		d.logger.Printf("removed owner %s after finalization warning: %v", shortServerID(prepared.result.ServerID), prepared.finalizationErr)
	} else {
		d.logger.Printf("removed owner %s", shortServerID(prepared.result.ServerID))
	}
	return supErr
}

func (d *Daemon) forgetOwnerIfCurrent(serverID string, expected *OwnerEntry, reason ownerRemovalReason) ownerRemovalResult {
	result, err := d.removeOwnerIfCurrent(serverID, expected, reason, false)
	if err != nil {
		d.logger.Printf("owner %s removal deferred: %v", shortServerID(serverID), err)
	}
	return result
}

func (d *Daemon) deleteOwnerEntryLocked(serverID string) {
	delete(d.owners, serverID)
	d.cleanupForcedIsolatedRetryCounterLocked(serverID)
}

func (d *Daemon) cleanupForcedIsolatedRetryCounterLocked(serverID string) {
	base, ok := retryCounterBaseForServerID(serverID)
	if !ok {
		return
	}
	if _, exists := d.forcedIsolatedRetryCounters.Load(base); !exists {
		return
	}
	for sid := range d.owners {
		if sid == base || strings.HasPrefix(sid, base+"-r") {
			return
		}
	}
	d.forcedIsolatedRetryCounters.Delete(base)
}

func retryCounterBaseForServerID(serverID string) (string, bool) {
	if matches := retrySidPattern.FindStringSubmatch(serverID); matches != nil {
		return matches[1], true
	}
	if strings.HasPrefix(serverID, "isolated-") {
		return serverID, true
	}
	return "", false
}

func (d *Daemon) recordOwnerRemovalLocked(result ownerRemovalResult) {
	if d.ownerRemoval.ByReason == nil {
		d.ownerRemoval.ByReason = make(map[ownerRemovalReason]uint64)
	}
	d.ownerRemoval.Total++
	d.ownerRemoval.ByReason[result.Reason]++
	d.ownerRemoval.PendingTokensRemoved += uint64(result.PendingTokensRemoved)
	d.ownerRemoval.BoundHistoryRemoved += uint64(result.BoundHistoryRemoved)
}

func terminationHintForRemoval(reason ownerRemovalReason) TerminationHint {
	switch reason {
	case ownerRemovalReasonOperatorHard, ownerRemovalReasonOperatorSoft:
		return HintOperatorStop
	case ownerRemovalReasonIdle:
		return HintIdleEviction
	case ownerRemovalReasonHandoff:
		return HintPlannedHandoff
	default:
		return HintNone
	}
}
