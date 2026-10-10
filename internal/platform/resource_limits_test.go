package platform

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestAccountRuleLimitAcrossNodesAndImport(t *testing.T) {
	f := setup(t)
	g1, n1 := f.node()
	g2, n2 := f.node()
	f.s.opts.ResourceLimits = func(context.Context, *sql.Tx, string) (contract.ResourceLimits, error) {
		return contract.ResourceLimits{MaxRules: 2}, nil
	}
	var wg sync.WaitGroup
	type result struct {
		code int
		body string
	}
	results := make(chan result, 8)
	for i := range 8 {
		wg.Go(func() {
			g, n := g1, n1
			if i%2 == 0 {
				g, n = g2, n2
			}
			rule := ruleFor(g, n)
			rule.Enabled = false
			rule.Listen = fmt.Sprintf(":%d", 20010+i)
			response := f.req("POST", "/rules", rule, "")
			results <- result{response.Code, response.Body.String()}
		})
	}
	wg.Wait()
	close(results)
	success := 0
	for response := range results {
		if response.code == 201 {
			success++
		} else if response.code != 409 || !strings.Contains(response.body, "plan rule limit reached") {
			t.Fatalf("unexpected concurrent rule response: %d %s", response.code, response.body)
		}
	}
	if success != 2 {
		t.Fatal("account limit raced across groups", success)
	}
	rule := ruleFor(g1, n1)
	rule.Enabled = false
	rule.Listen = ":20099"
	task := read[Task](t, f.req("POST", "/tasks/rules/import", map[string]any{"rules": []contract.Rule{rule}, "idempotency_key": "over-plan"}, ""), 202)
	if _, err := f.s.RunTasks(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	got := read[Task](t, f.req("GET", "/tasks/"+task.ID, nil, ""), 200)
	if got.Status != "completed_with_errors" {
		t.Fatal("import bypassed plan limit", got)
	}
}

func TestACKReplayAndCapabilityRefresh(t *testing.T) {
	f := setup(t)
	_, n := f.node()
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	ack := contract.Ack{Version: cfg.Version, AppliedVersion: cfg.Version, Capabilities: []string{"resource-limits-v1"}}
	if rr := f.req("POST", "/agent/ack", ack, n.Token); rr.Code != 204 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	next := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if next.Version != cfg.Version+1 {
		t.Fatal("capability change did not publish a new configuration", next.Version)
	}
	ack.Version = next.Version
	ack.AppliedVersion = next.Version
	for range 3 {
		rr := f.req("POST", "/agent/ack", ack, n.Token)
		if rr.Code != 204 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	ack.Version++
	ack.AppliedVersion++
	if rr := f.req("POST", "/agent/ack", ack, n.Token); rr.Code != 409 {
		t.Fatal("unmatched ACK accepted", rr.Code)
	}
}
