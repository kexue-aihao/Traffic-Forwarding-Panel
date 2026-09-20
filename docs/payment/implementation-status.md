# Payment and quota implementation status

This is a first implementation slice, not completed product acceptance.

- EPay: classic submit.php / MD5 callback implemented with local signing, tampering and replay tests. Provider version and merchant end-to-end evidence remain required; active query/reconciliation not implemented yet.
- Original EPUSDT and Bepusdt: mandatory, not yet implemented. Source discovery on 2026-09-20 resolved assimon/epusdt master to aed4a970a28d734c8a35499496604b868a24ef7f via GitHub tree API; this discovery does NOT establish a supported version or verify API behavior. API source inspection remains pending.
- LightCountry/TokenPay and Cryptomus: mandatory, not yet implemented; protocol version locking and merchant tests pending.
- Cyber: mandatory external dependency blocked on exact provider identity and versioned API documentation. No invented endpoint or signature.
- Refunds, payment attempts, outbox/reconciliation, coupons and commissions remain future implementation work, not covered by first slice.

## Quota integration

Commerce Service implements Allocate(ctx, tx, userID, ruleID, nodeID) and AllocateWithMultiplier with a final exact rational multiplier string. Both use the caller's SQL transaction. Multiply ingress and egress group factors before allocation; default Allocate is multiplier 1 only. Returned Lease.Bytes is RAW bidirectional application payload allowance. Allocated entitlement budget is weighted bytes.

SettleUsage(ctx, tx, contract.UsageRecord) validates persisted lease bindings and charges the lease's original entitlement, including reports delayed past a renewal. Duplicate identical records are accepted, changed repeats rejected. Agent must use globally unique usage IDs and non-overlapping delta records; period counters must not be resent as new deltas.

Current leases last at most five minutes and reserve at most 16 MiB weighted budget. Reservations are deliberately NOT reclaimed solely because a lease expires: delayed usage may still arrive. This is a significant availability limitation, not final production quota behavior. Implement authenticated final usage/lease surrender and persistent Agent spend state, release only proven unspent budget, and issue consumption-driven successor batches. Allocate cannot be called on every config poll; control plane must reuse current valid allocations. Config must replace old entitlement leases after purchases; runtime must stop stale grants at expiry. Offline propagation is not instantaneous.

Money uses CNY integer cents. Successful purchase creates a new cycle starting now, expiry by Asia/Shanghai calendar-month addition with end-day clamping. It does not extend old expiry or grant another reset on calendar month day 1. Old ledger and usage facts remain immutable.

SQLite tests passed locally. PostgreSQL/MySQL SQL syntax was designed portably but live database integration has NOT been run. Merchant payments have NOT been tested. API collection paging is currently capped at 100 for wallet ledger/orders and needs full shared pagination integration.
