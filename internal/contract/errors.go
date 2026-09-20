package contract

import "errors"

// ErrEntitlementUnavailable denies a rule without blocking revocation/config
// changes for other rules on the same node. Database failures are distinct.
var ErrEntitlementUnavailable = errors.New("entitlement unavailable")
