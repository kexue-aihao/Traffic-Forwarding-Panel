package platform

import (
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestProbeConnectionsLegacyAndUnavailable(t *testing.T) {
	f := setup(t)
	f.offlineGeo()
	group := read[contract.Group](t, f.req("POST", "/groups", map[string]any{"name": "connections", "port_min": 30000, "port_max": 31000, "multiplier": "1"}, ""), 201)
	node := f.nodeIn(group.ID, "connections")
	now := time.Now().UTC()
	for i, counters := range []map[string]any{
		{"tcp_connections": "42", "udp_connections": "0"},
		{}, // Legacy Agents omit both counters.
		{"tcp_connections": nil, "udp_connections": "3"},
	} {
		body := map[string]any{"node_id": node.NodeID, "sampled_at": now.Add(time.Duration(i) * time.Millisecond)}
		for key, value := range counters {
			body[key] = value
		}
		if rr := f.req("POST", "/agent/probe", body, node.Token); rr.Code != 204 {
			t.Fatalf("probe rejected: %d %s", rr.Code, rr.Body.String())
		}
		page := read[map[string]any](t, f.req("GET", "/probes", nil, ""), 200)
		entry := page["items"].([]any)[0].(map[string]any)
		for _, key := range []string{"tcp_connections", "udp_connections"} {
			if value, exists := entry[key]; !exists || value != counters[key] {
				t.Fatalf("%s = %v, want %v (missing must be null, not a previous count)", key, value, counters[key])
			}
		}
	}
	for _, invalid := range []any{"-1", "not-a-count", 42} {
		body := map[string]any{"node_id": node.NodeID, "sampled_at": now, "tcp_connections": invalid}
		if rr := f.req("POST", "/agent/probe", body, node.Token); rr.Code != 400 {
			t.Fatalf("invalid count %v accepted: %d", invalid, rr.Code)
		}
	}
}
