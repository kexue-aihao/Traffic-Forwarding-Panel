package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
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
				headers := map[string]string{}
				if inline {
					headers["X-TFP-Lease-Config"] = "1"
				}
				return leaseSnapshotRequest(t, f, path, body, headers, status)
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

func leaseSnapshotRequest(t *testing.T, f *fixture, path string, body any, headers map[string]string, status int) []byte {
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
	for key, value := range headers {
		req.Header.Set(key, value)
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

func TestRetirementRefillsOnlyActiveAuthorizedTCPWithinCommercialBounds(t *testing.T) {
	for _, tc := range []struct {
		name, multiplier, hint, network string
		quota, used, standbyBudget      int64
		revoked, legacy                 bool
	}{
		{name: "unit", multiplier: "1", hint: "134217728", network: "tcp", used: 1 << 20, standbyBudget: 128 << 20},
		{name: "half", multiplier: "1/2", hint: "134217728", network: "tcp", used: 1 << 20, standbyBudget: 64 << 20},
		{name: "three_halves", multiplier: "3/2", hint: "134217728", network: "tcp", used: 1 << 20, standbyBudget: 192 << 20},
		{name: "account_cap", multiplier: "3", hint: "268435456", network: "tcp", used: 1 << 20, standbyBudget: 256 << 20},
		{name: "remaining_quota", multiplier: "1", hint: "134217728", network: "tcp", quota: 200 << 20, used: 1 << 20, standbyBudget: 71 << 20},
		{name: "idle", multiplier: "1", hint: "134217728", network: "tcp"},
		{name: "oversized", multiplier: "1", hint: "268435457", network: "tcp", used: 1 << 20},
		{name: "malformed", multiplier: "1", hint: "garbage", network: "tcp", used: 1 << 20},
		{name: "revoked", multiplier: "1", hint: "134217728", network: "tcp", used: 1 << 20, revoked: true},
		{name: "udp", multiplier: "1", hint: "134217728", network: "udp", used: 1 << 20},
		{name: "legacy", multiplier: "1", hint: "134217728", network: "tcp", used: 1 << 20, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tunnel.Client{})
			quota := tc.quota
			if quota == 0 {
				quota = 4 << 30
			}
			if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE commerce_entitlements SET quota=? WHERE id=?"), quota, f.ent.ID); err != nil {
				t.Fatal(err)
			}
			f.group.Multiplier = tc.multiplier
			f.group = decode[contract.Group](t, f.request("PUT", "/groups/"+f.group.ID, f.group, f.admin, http.StatusOK))
			tcp, udp := targets(t)
			target := tcp
			if tc.network == "udp" {
				target = udp
			}
			rule := f.rule(tc.network, "direct", target, nil)
			f.sync()
			cfg := f.store.Config()
			rule = cfg.Rules[0]
			first := rule.Lease.ID
			headers := map[string]string{"X-TFP-Lease-Config": "1"}
			input := map[string]any{"rule_id": rule.ID, "lease_id": first, "raw_budget": "134217728"}
			response := decode[struct {
				Config contract.Config `json:"config"`
			}](t, leaseSnapshotRequest(t, f, "/agent/leases/prefetch", input, headers, 200))
			promoted := response.Config.Rules[0].StandbyLease.ID
			if err := f.agent.Runtime.Apply(response.Config, true); err != nil {
				t.Fatal(err)
			}
			if tc.used > 0 {
				if err := f.store.Charge(rule, cfg.ValidUntil, false, int(tc.used)); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.store.Retire(first); err != nil {
				t.Fatal(err)
			}
			pending := f.store.Pending()
			if len(pending) > 0 {
				leaseSnapshotRequest(t, f, "/agent/usage", contract.UsageBatch{Records: pending}, nil, http.StatusOK)
				ids := make([]string, 0, len(pending))
				for _, u := range pending {
					ids = append(ids, u.ID)
				}
				if err := f.store.Confirm(ids); err != nil {
					t.Fatal(err)
				}
			}
			if tc.revoked {
				if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE cp_users SET disabled=1 WHERE id=?"), f.owner.ID); err != nil {
					t.Fatal(err)
				}
			}
			headers["X-TFP-Lease-Prefetch-Budget"] = tc.hint
			if tc.legacy {
				delete(headers, "X-TFP-Lease-Config")
			}
			status := http.StatusOK
			if tc.legacy {
				status = http.StatusNoContent
			}
			retirement := map[string]any{"lease_id": first, "used_bytes": strconv.FormatInt(tc.used, 10)}
			body := leaseSnapshotRequest(t, f, "/agent/leases/retire", retirement, headers, status)
			wantLeases := int64(2)
			if tc.standbyBudget > 0 {
				wantLeases++
				response = decode[struct {
					Config contract.Config `json:"config"`
				}](t, body)
				updated := response.Config.Rules[0]
				if updated.Lease.ID != promoted || updated.StandbyLease == nil {
					t.Fatal("did not promote and refill in one response")
				}
				if err := f.agent.Runtime.Apply(response.Config, true); err != nil {
					t.Fatal("refilled snapshot bypassed normal validation", err)
				}
				if got := f.number("SELECT budget FROM commerce_lease_reservations WHERE lease_id=?", updated.StandbyLease.ID); got != tc.standbyBudget {
					t.Fatal("standby lost multiplier or exceeded remaining quota", got, tc.standbyBudget)
				}
				if got := f.number("SELECT COALESCE(SUM(budget),0) FROM commerce_lease_reservations WHERE closed=0"); got > 512<<20 {
					t.Fatal("exceeded account reservation bound", got)
				}
			}
			if got := f.number("SELECT COUNT(*) FROM commerce_leases WHERE rule_id=?", rule.ID); got != wantLeases {
				t.Fatal("unexpected allocation for retirement hint", got, wantLeases)
			}
			if f.number("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?", first) != 1 {
				t.Fatal("refill did not close settled primary")
			}
			wantUsed := tc.used
			switch tc.multiplier {
			case "1/2":
				wantUsed /= 2
			case "3/2":
				wantUsed = wantUsed * 3 / 2
			case "3":
				wantUsed *= 3
			}
			if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != wantUsed {
				t.Fatal("retirement/refill changed billed consumption", got, wantUsed)
			}
			outstanding := f.number("SELECT COALESCE(SUM(budget),0) FROM commerce_lease_reservations WHERE closed=0")
			before := f.number("SELECT allocated FROM commerce_entitlements WHERE id=?", f.ent.ID)
			if before != outstanding+wantUsed {
				t.Fatal("retirement did not refund unused allocation exactly", before, outstanding, wantUsed)
			}
			leaseSnapshotRequest(t, f, "/agent/leases/retire", retirement, headers, status)
			if f.number("SELECT COUNT(*) FROM commerce_leases WHERE rule_id=?", rule.ID) != wantLeases || f.number("SELECT allocated FROM commerce_entitlements WHERE id=?", f.ent.ID) != before {
				t.Fatal("replay changed reservations")
			}
			if tc.standbyBudget > 0 {
				// Discard the unused next standby, leaving the promoted primary
				// idle. Replaying its predecessor must not refill this newer rule.
				updated := response.Config.Rules[0]
				leaseSnapshotRequest(t, f, "/agent/leases/retire", map[string]any{"lease_id": updated.StandbyLease.ID, "used_bytes": "0"}, map[string]string{"X-TFP-Lease-Config": "1"}, http.StatusOK)
				before = f.number("SELECT allocated FROM commerce_entitlements WHERE id=?", f.ent.ID)
				leaseSnapshotRequest(t, f, "/agent/leases/retire", retirement, headers, status)
				if f.number("SELECT COUNT(*) FROM commerce_leases WHERE rule_id=?", rule.ID) != wantLeases || f.number("SELECT allocated FROM commerce_entitlements WHERE id=?", f.ent.ID) != before {
					t.Fatal("stale retirement refilled an idle successor")
				}
			}
		})
	}
}
