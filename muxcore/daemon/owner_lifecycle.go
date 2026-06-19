package daemon

import (
	"fmt"
	"strings"
	"time"
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

	entry.terminationHint = terminationHintForRemoval(reason)
	result.PendingTokensRemoved = entry.Owner.SessionMgr().RemovePendingForOwner(serverID)
	result.BoundHistoryRemoved = entry.Owner.SessionMgr().RemoveBoundForOwner(serverID)
	d.recordOwnerRemovalLocked(result)
	d.deleteOwnerEntryLocked(serverID)
	result.Removed = true
	token := entry.serviceToken
	ownerRef := entry.Owner
	d.mu.Unlock()

	var supErr error
	if d.supervisor != nil {
		if err := d.supervisor.RemoveAndWait(token, 2*time.Second); err != nil {
			action := "remove"
			if soft {
				action = "soft-remove"
			}
			supErr = fmt.Errorf("%s owner %s from supervisor: %w", action, shortServerID(serverID), err)
			d.logger.Printf("warning: %v", supErr)
		}
	}

	if soft {
		exitCode, err := ownerRef.SoftShutdown(30 * time.Second)
		if err != nil {
			d.logger.Printf("soft-removed owner %s: forced kill (exit=%d err=%v)", shortServerID(serverID), exitCode, err)
		} else if exitCode == 0 {
			d.logger.Printf("soft-removed owner %s: upstream exited cleanly (code 0)", shortServerID(serverID))
		} else {
			d.logger.Printf("soft-removed owner %s: upstream exited with code %d", shortServerID(serverID), exitCode)
		}
	} else {
		ownerRef.Shutdown()
		d.logger.Printf("removed owner %s", shortServerID(serverID))
	}

	return result, supErr
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
