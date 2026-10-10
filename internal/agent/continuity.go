package agent

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const renewalWait = 30 * time.Second

// A replacement allocation changes accounting, not the destination or policy.
// Limit changes still close existing sessions so a downgrade applies at once.
func sameForwardingRule(a, b contract.Rule) bool {
	if a.Lease == nil || b.Lease == nil {
		return reflect.DeepEqual(a, b)
	}
	if a.Lease.Limits != b.Lease.Limits {
		return false
	}
	a.Lease, b.Lease = nil, nil
	a.StandbyLease, b.StandbyLease = nil, nil
	a.LeasePipeline, b.LeasePipeline = false, false
	if !sameExistingCandidates(a.RouteCandidates, b.RouteCandidates) {
		return false
	}
	a.RouteCandidates, b.RouteCandidates = nil, nil
	return reflect.DeepEqual(a, b)
}

// Adding candidates and changing weights affect new connections. Removing an
// authorized route or changing its credentials/policy revokes existing flows.
func sameExistingCandidates(a, b []contract.RouteCandidate) bool {
	if len(a) == 0 || len(b) == 0 {
		return reflect.DeepEqual(a, b)
	}
	for _, old := range a {
		found := false
		for _, next := range b {
			if old.ID == next.ID {
				old.Weight, next.Weight = 0, 0
				if !reflect.DeepEqual(old, next) {
					return false
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func transientMeterError(err error) bool {
	return errors.Is(err, errLeaseUnavailable) || errors.Is(err, errSpoolFull) || errors.Is(err, errCreditUnavailable)
}

func (b *binding) liveRule(ctx context.Context, id string) (contract.Rule, time.Time, <-chan struct{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return contract.Rule{}, time.Time{}, nil, err
	}
	if b.closed {
		return contract.Rule{}, time.Time{}, nil, errors.New("listener closed")
	}
	if b.rule.ID == id {
		return b.rule, b.until, b.changed, nil
	}
	for _, route := range b.routes {
		if route.rule.ID == id {
			return route.rule, route.until, b.changed, nil
		}
	}
	return contract.Rule{}, time.Time{}, nil, errors.New("rule removed")
}

func waitMeter(ctx context.Context, config, state <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-config:
	case <-state:
	}
	return nil
}

func waitCreditMeter(ctx context.Context, config, state <-chan struct{}) error {
	// A full writer queue must not leave TCP asleep after its refill failed to
	// enqueue. Retry slowly when there is no durable/config notification.
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-config:
	case <-state:
	case <-timer.C:
	}
	return nil
}

func (b *binding) awaitAvailable(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, renewalWait)
	defer cancel()
	for {
		progress := b.runtime.Store.changes()
		rule, until, changed, err := b.liveRule(ctx, id)
		if err != nil {
			return err
		}
		err = b.runtime.Store.Available(rule, until)
		if !transientMeterError(err) {
			return err
		}
		b.runtime.Store.requestUsage()
		if err := waitMeter(ctx, changed, progress); err != nil {
			return err
		}
	}
}

func (b *binding) chargeCurrent(ctx context.Context, id, network string, up bool, n int) error {
	if network == "udp" {
		rule, until, _, err := b.liveRule(ctx, id)
		if err != nil {
			return err
		}
		if rule.UDP != nil && rule.UDP.CreditWindows {
			return b.runtime.Store.TryUDPCharge(rule, until, up, n)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, renewalWait)
	defer cancel()
	for {
		progress := b.runtime.Store.changes()
		rule, until, changed, err := b.liveRule(ctx, id)
		if err != nil {
			return err
		}
		if network == "tcp" || network == "udp" && rule.UDP != nil && rule.UDP.CreditWindows {
			err = b.runtime.Store.TryCreditCharge(rule, until, up, n)
		} else {
			err = b.runtime.Store.chargeContext(ctx, rule, until, up, n)
		}
		if !transientMeterError(err) {
			return err
		}
		b.runtime.Store.requestUsage()
		// A shared UDP listener must not wait on one peer's allocation.
		if network == "udp" {
			return err
		}
		if err := waitCreditMeter(ctx, changed, progress); err != nil {
			return err
		}
	}
}
