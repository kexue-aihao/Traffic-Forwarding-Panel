package agent

import (
	"errors"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// Stop issuing allocations rather than allowing an offline spool to grow
// indefinitely. Confirmed, expired history is pruned in an atomic checkpoint.
const maxRetainedLeases = 16384

func (s *Store) pruneLeaseHistoryLocked() int {
	referenced := map[string]bool{}
	for _, r := range s.state.Config.Rules {
		for _, l := range ruleLeases(r) {
			if l != nil {
				referenced[l.ID] = true
			}
		}
	}
	for _, u := range s.state.Pending {
		referenced[u.LeaseID] = true
	}
	for _, w := range s.state.Windows {
		referenced[w.LeaseID] = true
	}
	n := 0
	for id, l := range s.state.Leases {
		// Keep an unexpired tombstone: dropping it could spend its budget twice.
		if s.state.Retired[id] && !referenced[id] && !time.Now().Before(l.ExpiresAt) {
			delete(s.state.Leases, id)
			delete(s.state.Used, id)
			delete(s.state.Retired, id)
			n++
		}
	}
	return n
}

func (s *Store) validateSnapshot() error {
	if len(s.state.Pending)+len(s.state.Windows) > MaxPendingRecords || len(s.state.Leases) > maxRetainedLeases || len(s.state.Used) > maxRetainedLeases || len(s.state.Retired) > maxRetainedLeases {
		return errors.New("durable state capacity exceeded")
	}
	for id, l := range s.state.Leases {
		if id == "" || id != l.ID || l.Bytes <= 0 || l.ExpiresAt.IsZero() || s.state.Used[id] < 0 || s.state.Used[id] > l.Bytes {
			return errors.New("invalid durable lease budget")
		}
	}
	for id := range s.state.Used {
		if _, ok := s.state.Leases[id]; !ok {
			return errors.New("durable usage lease missing")
		}
	}
	for id := range s.state.Retired {
		if _, ok := s.state.Leases[id]; !ok {
			return errors.New("durable retirement lease missing")
		}
	}
	ids, pendingBytes := map[string]bool{}, map[string]int64{}
	for _, u := range s.state.Pending {
		l, ok := s.state.Leases[u.LeaseID]
		if !ok || u.ID == "" || ids[u.ID] || !u.ValidWindowMetadata() || u.NodeID != s.state.Identity.NodeID || u.EntitlementID != l.EntitlementID || u.UploadBytes < 0 || u.DownloadBytes < 0 || u.UploadBytes > l.Bytes || u.DownloadBytes > l.Bytes-u.UploadBytes || u.EndedAt.Before(u.StartedAt) || u.EndedAt.After(l.ExpiresAt) {
			return errors.New("invalid durable pending usage")
		}
		ids[u.ID] = true
		n := u.UploadBytes + u.DownloadBytes
		if n > s.state.Used[l.ID]-pendingBytes[l.ID] {
			return errors.New("pending usage exceeds durable spent budget")
		}
		pendingBytes[l.ID] += n
	}
	liability, rules, owners, pools := map[string]int64{}, map[string]int64{}, map[string]int64{}, map[creditPoolKey]int{}
	for id, w := range s.state.Windows {
		l, ok := s.state.Leases[w.LeaseID]
		_, retired := s.state.Retired[w.LeaseID]
		if !ok || retired || w.ID != id || len(w.ID) != 32 || w.RuleID == "" || w.UserID == "" || w.Closed || w.Capacity <= 0 || w.Capacity > udpCreditSize || w.Confirmed < 0 || w.Confirmed > w.Capacity || w.Confirmed > 0 && w.Sequence == 0 || w.Sequence == ^uint64(0) || w.StartedAt.IsZero() || !w.Until.After(w.StartedAt) || w.Until.After(l.ExpiresAt) {
			return errors.New("invalid durable credit window")
		}
		left := w.Capacity - w.Confirmed
		if left > l.Bytes-s.state.Used[l.ID]-liability[l.ID] {
			return errors.New("durable credit exceeds lease budget")
		}
		liability[l.ID] += left
		rules[w.RuleID] += left
		owners[w.UserID] += left
		pools[creditKey(w.RuleID, w.Upload)]++
		if rules[w.RuleID] > maxRuleCredits || owners[w.UserID] > maxAccountCredits || pools[creditKey(w.RuleID, w.Upload)] > 2 {
			return errors.New("durable credit responsibility exceeds bounds")
		}
	}
	return nil
}

func (s *Store) validateLeaseCapacity(incoming []*contract.Lease) error {
	count := len(s.state.Leases)
	newIDs := map[string]bool{}
	for _, l := range incoming {
		if l == nil {
			continue
		}
		if _, exists := s.state.Leases[l.ID]; !exists && !newIDs[l.ID] {
			newIDs[l.ID] = true
			count++
		}
	}
	if count > maxRetainedLeases {
		return errSpoolFull
	}
	return nil
}
