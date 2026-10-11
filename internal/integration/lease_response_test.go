package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func TestLeaseResponsesPublishValidatedSnapshotsAndPreserveBudgets(t *testing.T) {
	for _, billing := range []struct {
		multiplier string
		budget     int64
	}{{"1", 128 << 20}, {"1/2", 64 << 20}, {"3/2", 192 << 20}} {
		t.Run(billing.multiplier, func(t *testing.T) {
			f := newFixture(t, tunnel.Client{})
			if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE commerce_entitlements SET quota=? WHERE id=?"), int64(4<<30), f.ent.ID); err != nil {
				t.Fatal(err)
			}
			f.group.Multiplier = billing.multiplier
			f.group = decode[contract.Group](t, f.request("PUT", "/groups/"+f.group.ID, f.group, f.admin, http.StatusOK))
			target, _ := targets(t)
			rule := f.rule("tcp", "direct", target, nil)
			f.sync()
			first := f.lease(rule.ID).ID
			request := func(path string, body any, inline bool, status int) []byte {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/v1"+path, bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+f.store.Identity().Token)
				req.Header.Set("Content-Type", "application/json")
				if inline {
					req.Header.Set("X-TFP-Lease-Config", "1")
				}
				res, err := f.http.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				data, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				if res.StatusCode != status {
					t.Fatalf("lease operation status %d: %s", res.StatusCode, data)
				}
				return data
			}
			const budget = 128 << 20
			input := map[string]any{"rule_id": rule.ID, "lease_id": first, "raw_budget": "134217728"}
			legacy := decode[map[string]json.RawMessage](t, request("/agent/leases/prefetch", input, false, http.StatusOK))
			if len(legacy) != 1 || legacy["lease_id"] == nil {
				t.Fatal("legacy prefetch response changed")
			}
			type leaseResponse struct {
				LeaseID string           `json:"lease_id"`
				Config  *contract.Config `json:"config"`
			}
			response := decode[leaseResponse](t, request("/agent/leases/prefetch", input, true, http.StatusOK))
			if response.Config == nil || response.Config.NodeID != f.store.Identity().NodeID || len(response.Config.Rules) != 1 {
				t.Fatal("prefetch omitted its authorized full snapshot")
			}
			updated := response.Config.Rules[0]
			if updated.ID != rule.ID || updated.Target != target || updated.StandbyLease == nil || updated.StandbyLease.ID != response.LeaseID || updated.StandbyLease.Bytes != budget {
				t.Fatal("snapshot lost policy or published the wrong standby")
			}
			if err := f.agent.Runtime.Apply(*response.Config, true); err != nil {
				t.Fatal("inline snapshot failed normal Agent validation", err)
			}
			retirement := map[string]any{"lease_id": first, "used_bytes": "0"}
			response = decode[leaseResponse](t, request("/agent/leases/retire", retirement, true, http.StatusOK))
			if response.Config == nil || len(response.Config.Rules) != 1 || response.Config.Rules[0].Lease.ID != updated.StandbyLease.ID {
				t.Fatal("retirement did not publish the promoted standby")
			}
			if err := f.agent.Runtime.Apply(*response.Config, true); err != nil {
				t.Fatal(err)
			}
			// Losing the next standby must preserve the learned, bounded allocation.
			retirement["lease_id"] = updated.StandbyLease.ID
			response = decode[leaseResponse](t, request("/agent/leases/retire", retirement, true, http.StatusOK))
			if response.Config == nil || len(response.Config.Rules) != 1 || response.Config.Rules[0].Lease.Bytes != budget || response.Config.Rules[0].Lease.ID == updated.StandbyLease.ID {
				t.Fatal("replacement restarted a learned lease at the initial budget")
			}
			if err := f.agent.Runtime.Apply(*response.Config, true); err != nil {
				t.Fatal(err)
			}
			request("/agent/leases/retire", retirement, false, http.StatusNoContent)
			if f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID) != 0 {
				t.Fatal("unused allocations changed billed usage")
			}
			if f.number("SELECT COUNT(*) FROM cp_lease_retirements WHERE id IN (?,?)", first, updated.StandbyLease.ID) != 2 {
				t.Fatal("retirement replay changed persisted facts")
			}
			if got := f.number("SELECT budget FROM commerce_lease_reservations WHERE lease_id=?", response.Config.Rules[0].Lease.ID); got != billing.budget {
				t.Fatal("replacement lost the current billing multiplier", got, billing.budget)
			}
			if got := f.number("SELECT allocated FROM commerce_entitlements WHERE id=?", f.ent.ID); got != billing.budget {
				t.Fatal("retired reservations were not reclaimed exactly", got, billing.budget)
			}
		})
	}
}
