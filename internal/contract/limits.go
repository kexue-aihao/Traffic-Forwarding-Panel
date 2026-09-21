package contract

import "errors"

// ResourceLimits are snapshotted at purchase. Zero means no commercial limit;
// the Agent's independent safety bounds still apply. Runtime limits aggregate
// all of a user's rules on one node, never across unrelated nodes.
type ResourceLimits struct {
	MaxRules              int   `json:"max_rules"`
	MaxConnectionsPerNode int   `json:"max_connections_per_node"`
	MaxIPsPerNode         int   `json:"max_ips_per_node"`
	BytesPerSecondPerNode int64 `json:"bytes_per_second_per_node,string"`
}

func (l ResourceLimits) Validate() error {
	if l.MaxRules < 0 || l.MaxRules > 100000 || l.MaxConnectionsPerNode < 0 || l.MaxConnectionsPerNode > 1000000 || l.MaxIPsPerNode < 0 || l.MaxIPsPerNode > 1000000 || l.BytesPerSecondPerNode < 0 || l.BytesPerSecondPerNode > 1000000000000 {
		return errors.New("invalid resource limits")
	}
	return nil
}
