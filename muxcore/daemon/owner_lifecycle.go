package daemon

import (
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
	result   ownerRemovalResult
	token    suture.ServiceToken
	ownerRef *owner.Owner
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
	result := ownerRemovalResult{ServerID: serverID, Reason: reason, Soft: soft}

	d.mu.Lock()
	entry, ok := d.owners[serverID]
	if !ok {
		d.mu.Unlock()
		return result, fmt.Errorf("server %s not found", serverID)
	}
	if expected != nil && entry != expected {
		d.mu.Unlock()
		return result, nil
	}
	if entry.Owner == nil {
		d.mu.Unlock()
		return result, fmt.Errorf("server %s is still being created", serverID)
	}

	prepared := d.prepareOwnerRemovalLocked(serverID, entry, reason, soft)
	d.mu.Unlock()

	return prepared.result, d.finishOwnerRemoval(prepared)
}

func (d *Daemon) removeOwnerIfCurrentAndZeroIdle(serverID string, expected *OwnerEntry, zeroAt time.Time, idleTimeout time.Duration) (ownerRemovalResult, bool, error) {
	result := ownerRemovalResult{ServerID: serverID, Reason: ownerRemovalReasonIdle, Soft: true}

	d.mu.Lock()
	entry, ok := d.owners[serverID]
	if !ok || (expected != nil && entry != expected) || entry.Owner == nil {
		d.mu.Unlock()
		return result, false, nil
	}
	if !entry.LastSession.Equal(zeroAt) {
		d.mu.Unlock()
		return result, false, nil
	}

	sample := evictionSample{
		Sessions:             entry.Owner.SessionCount(),
		Persistent:           entry.Persistent,
		PendingRequests:      entry.Owner.PendingRequests(),
		ActiveProgressTokens: entry.Owner.ActiveProgressTokens(),
		HasBusyWork:          entry.Owner.HasActiveBusyWork(),
		UpstreamDead:         entry.Owner.UpstreamDead(),
		IdleTimeout:          idleTimeout,
		OwnerIdleOverride:    entry.Owner.IdleTimeout(),
		LastSession:          entry.LastSession,
		LastActivity:         entry.Owner.LastActivity(),
		IsolatedClassified:   entry.Owner.IsClassifiedIsolated(),
	}
	decision := shouldEvict(sample, time.Now(), idleTimeout, 0)
	if !decision.evict {
		d.mu.Unlock()
		return result, false, nil
	}

	reason := ownerRemovalReasonIdle
	soft := true
	if decision.reason == "zombie" {
		reason = ownerRemovalReasonZombie
		soft = false
	}
	prepared := d.prepareOwnerRemovalLocked(serverID, entry, reason, soft)
	d.mu.Unlock()

	return prepared.result, true, d.finishOwnerRemoval(prepared)
}

func (d *Daemon) prepareOwnerRemovalLocked(serverID string, entry *OwnerEntry, reason ownerRemovalReason, soft bool) preparedOwnerRemoval {
	result := ownerRemovalResult{ServerID: serverID, Reason: reason, Soft: soft}
	entry.terminationHint = terminationHintForRemoval(reason)
	result.PendingTokensRemoved = entry.Owner.SessionMgr().RemovePendingForOwner(serverID)
	result.BoundHistoryRemoved = entry.Owner.SessionMgr().RemoveBoundForOwner(serverID)
	d.recordOwnerRemovalLocked(result)
	d.deleteOwnerEntryLocked(serverID)
	result.Removed = true
	return preparedOwnerRemoval{
		result:   result,
		token:    entry.serviceToken,
		ownerRef: entry.Owner,
	}
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
		exitCode, err := prepared.ownerRef.SoftShutdown(30 * time.Second)
		if err != nil {
			d.logger.Printf("soft-removed owner %s: forced kill (exit=%d err=%v)", shortServerID(prepared.result.ServerID), exitCode, err)
		} else if exitCode == 0 {
			d.logger.Printf("soft-removed owner %s: upstream exited cleanly (code 0)", shortServerID(prepared.result.ServerID))
		} else {
			d.logger.Printf("soft-removed owner %s: upstream exited with code %d", shortServerID(prepared.result.ServerID), exitCode)
		}
	} else {
		prepared.ownerRef.Shutdown()
		d.logger.Printf("removed owner %s", shortServerID(prepared.result.ServerID))
	}

	return supErr
}

func (d *Daemon) forgetOwnerIfCurrent(serverID string, expected *OwnerEntry, reason ownerRemovalReason) ownerRemovalResult {
	result := ownerRemovalResult{ServerID: serverID, Reason: reason}
	d.mu.Lock()
	entry, ok := d.owners[serverID]
	if !ok || (expected != nil && entry != expected) {
		d.mu.Unlock()
		return result
	}
	if entry.Owner != nil {
		result.PendingTokensRemoved = entry.Owner.SessionMgr().RemovePendingForOwner(serverID)
		result.BoundHistoryRemoved = entry.Owner.SessionMgr().RemoveBoundForOwner(serverID)
	}
	d.recordOwnerRemovalLocked(result)
	d.deleteOwnerEntryLocked(serverID)
	d.mu.Unlock()
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
