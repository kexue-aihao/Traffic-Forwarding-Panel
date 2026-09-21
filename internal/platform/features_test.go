package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func TestImportPreviewRollbackAndVersionedPortUpdate(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	input := ruleFor(g, n)
	preview := read[struct {
		Items []ImportPreviewItem `json:"items"`
	}](t, f.req("POST", "/tasks/rules/preview", ImportRequest{Rules: []contract.Rule{input, input}}, ""), 200)
	if len(preview.Items) != 2 || preview.Items[0].Error != "" || preview.Items[1].Error == "" {
		t.Fatal(preview)
	}
	for _, table := range []string{"cp_rules", "cp_ports", "cp_rule_leases"} {
		var count int
		if err := f.s.Store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("preview wrote", table, count, err)
		}
	}
	rule := read[contract.Rule](t, f.req("POST", "/rules", input, ""), 201)
	input.Target = "127.0.0.1:8081"
	preview = read[struct {
		Items []ImportPreviewItem `json:"items"`
	}](t, f.req("POST", "/tasks/rules/preview", ImportRequest{Rules: []contract.Rule{input}, Mode: "update_by_port"}, ""), 200)
	if preview.Items[0].Rule == nil || preview.Items[0].Rule.Version != rule.Version {
		t.Fatal(preview)
	}
	confirmed := *preview.Items[0].Rule
	task := read[Task](t, f.req("POST", "/tasks/rules/import", ImportRequest{Rules: []contract.Rule{confirmed}, Mode: "update_by_port", Key: "update"}, ""), 202)
	if _, err := f.s.RunTasks(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	result := read[Task](t, f.req("GET", "/tasks/"+task.ID, nil, ""), 200)
	if result.Status != "completed" {
		t.Fatal(result)
	}
	read[Task](t, f.req("POST", "/tasks/rules/import", ImportRequest{Rules: []contract.Rule{confirmed}, Mode: "update_by_port", Key: "stale"}, ""), 202)
	if _, err := f.s.RunTasks(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := f.s.Store.DB.QueryRowContext(context.Background(), f.s.q("SELECT payload FROM cp_rules WHERE id=?"), rule.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got contract.Rule
	json.Unmarshal([]byte(raw), &got)
	if got.Version != 2 || got.Target != input.Target {
		t.Fatal(got)
	}
}

func TestManagedExitSelectionPricingRevocationAndCredentialIsolation(t *testing.T) {
	f := setup(t)
	entry, node := f.node()
	exitGroup, exitNode := f.node()
	entry.Multiplier = "3/2"
	read[contract.Group](t, f.req("PUT", "/groups/"+entry.ID, entry, ""), 200)
	exitGroup.Multiplier = "2"
	exitGroup = read[contract.Group](t, f.req("PUT", "/groups/"+exitGroup.ID, exitGroup, ""), 200)
	if err := f.s.Store.Write(context.Background(), storage.Normal, func(tx *sql.Tx) error {
		_, err := tx.Exec(f.s.q("UPDATE cp_nodes SET last_seen=? WHERE id=?"), time.Now().Unix(), exitNode.NodeID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	exit := read[contract.Exit](t, f.req("POST", "/exits", contract.Exit{Name: "exit", GroupID: exitGroup.ID, NodeID: exitNode.NodeID, Transport: "tls", Tunnel: contract.Tunnel{Endpoint: "exit.example:443", Token: "secret-exit-token-long", ServerName: "exit.example"}, Weight: 1, Enabled: true}, ""), 201)
	if exit.Tunnel.Token != "" {
		t.Fatal("exit credential disclosed")
	}
	input := ruleFor(entry, node)
	input.ExitGroupID = exitGroup.ID
	input.ExitID = "auto"
	// Inject a recording allocator to verify exact entry*exit pricing.
	recorder := &multiplierRecorder{}
	f.s.opts.Entitlements = recorder
	rule := read[contract.Rule](t, f.req("POST", "/rules", input, ""), 201)
	if rule.Tunnel != nil || rule.SelectedExitID != exit.ID || rule.BillingMultiplier != "3" || recorder.multiplier != "3" {
		t.Fatal(rule, recorder.multiplier)
	}
	var raw string
	f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_rules WHERE id=?"), rule.ID).Scan(&raw)
	if !strings.Contains(raw, "secret-exit-token-long") {
		t.Fatal("Agent credential not persisted")
	}
	// A managed route cannot be converted to a manual one by retaining its secret.
	rule.ExitGroupID = ""
	rule.Tunnel = &contract.Tunnel{Endpoint: "exit.example:443", ServerName: "exit.example"}
	if rr := f.req("PUT", "/rules/"+rule.ID, rule, ""); rr.Code != 409 {
		t.Fatal("managed credential inherited by manual route", rr.Body.String())
	}
	exit.Enabled = false
	read[contract.Exit](t, f.req("PUT", "/exits/"+exit.ID, exit, ""), 200)
	if err := f.s.refreshExits(context.Background(), node.NodeID); err != nil {
		t.Fatal(err)
	}
	f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_rules WHERE id=?"), rule.ID).Scan(&raw)
	var updated contract.Rule
	json.Unmarshal([]byte(raw), &updated)
	if !updated.ExitUnavailable || updated.Lease != nil {
		t.Fatal("disabled exit still funded", updated)
	}
}

type multiplierRecorder struct{ multiplier string }

func (a *multiplierRecorder) Allocate(ctx context.Context, tx *sql.Tx, user, rule, node string) (*contract.Lease, error) {
	return a.AllocateWithMultiplier(ctx, tx, user, rule, node, "1")
}
func (a *multiplierRecorder) AllocateWithMultiplier(ctx context.Context, tx *sql.Tx, user, rule, node, m string) (*contract.Lease, error) {
	a.multiplier = m
	return &contract.Lease{ID: id(), EntitlementID: "admin-test", Bytes: 1024, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestSiteRegistrationInvitationAndCaptchaReplay(t *testing.T) {
	f := setup(t)
	site := read[contract.SiteSettings](t, f.req("GET", "/site", nil, ""), 200)
	if site.Registration != "closed" {
		t.Fatal(site)
	}
	if rr := f.req("POST", "/auth/register", map[string]string{"username": "closed", "password": "long-password-123"}, ""); rr.Code != 403 {
		t.Fatal(rr.Code)
	}
	site.Registration = "invite"
	site = read[contract.SiteSettings](t, f.req("PUT", "/site", site, ""), 200)
	invite := read[map[string]string](t, f.req("POST", "/registration-invites", map[string]any{}, ""), 201)
	user := read[contract.User](t, f.req("POST", "/auth/register", map[string]string{"username": "registered", "password": "long-password-123", "invite": invite["code"]}, ""), 201)
	if user.Role != "user" {
		t.Fatal(user)
	}
	if rr := f.req("POST", "/auth/register", map[string]string{"username": "replay", "password": "long-password-123", "invite": invite["code"]}, ""); rr.Code != 409 {
		t.Fatal("invite replay", rr.Code)
	}
	site.Captcha = true
	site = read[contract.SiteSettings](t, f.req("PUT", "/site", site, ""), 200)
	image := read[map[string]string](t, f.req("GET", "/auth/captcha", nil, ""), 200)
	if !strings.HasPrefix(image["image"], "data:image/png;base64,") {
		t.Fatal(image)
	}
	// Seed the answer; verify that even an incorrect attempt consumes its token.
	key := strings.Repeat("a", 64)
	if err := f.s.Store.Write(context.Background(), storage.Normal, func(tx *sql.Tx) error {
		_, err := tx.Exec(f.s.q("INSERT INTO cp_captchas(id,answer_hash,expires_at) VALUES(?,?,?)"), key, digest(key+":123456"), time.Now().Add(time.Minute).Unix())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if f.s.verifyCaptcha(context.Background(), key, "654321") || f.s.verifyCaptcha(context.Background(), key, "123456") {
		t.Fatal("captcha replay")
	}
	if rr := f.req("POST", "/auth/login", map[string]string{"username": "registered", "password": "long-password-123"}, ""); rr.Code != 400 {
		t.Fatal("captcha bypass", rr.Code)
	}
}

func TestDiagnosticClaimFenceAndSanitizedResult(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	config := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	rr := f.req("POST", "/agent/ack", contract.Ack{Version: config.Version, AppliedVersion: config.Version, Capabilities: []string{"diagnostics-v1"}}, n.Token)
	if rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	rule := read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	diagnostic := read[contract.Diagnostic](t, f.req("POST", "/rules/"+rule.ID+"/network-diagnostic", map[string]any{}, ""), 202)
	dispatched := read[struct {
		Diagnostic *contract.Diagnostic `json:"diagnostic"`
	}](t, f.req("POST", "/agent/diagnostics", nil, n.Token), 200).Diagnostic
	if dispatched == nil || dispatched.ID != diagnostic.ID || dispatched.Claim == "" {
		t.Fatal(dispatched)
	}
	dispatched.Checks = []contract.DiagnosticCheck{{Stage: "exit_path", OK: false, Detail: "10.0.0.1 credential=secret", Milliseconds: 12}}
	claim := dispatched.Claim
	dispatched.Claim = "invalid"
	if rr = f.req("POST", "/agent/diagnostics/result", dispatched, n.Token); rr.Code != 409 {
		t.Fatal("invalid claim accepted", rr.Code)
	}
	dispatched.Claim = claim
	if rr = f.req("POST", "/agent/diagnostics/result", dispatched, n.Token); rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	result := read[contract.Diagnostic](t, f.req("GET", "/diagnostics/"+diagnostic.ID, nil, ""), 200)
	if result.Status != "completed" || result.Claim != "" || strings.Contains(strJSON(result), "10.0.0.1") || len(result.Checks) != 2 {
		t.Fatal(result)
	}
}
