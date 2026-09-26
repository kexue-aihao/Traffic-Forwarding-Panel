// Command generate emits the API document from shared wire types and route metadata.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/alerts"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/platform"
)

type schema = map[string]any

var schemas = map[string]any{}

func ref(name string) schema   { return schema{"$ref": "#/components/schemas/" + name} }
func scalar(t string) schema   { return schema{"type": t} }
func nullable(v schema) schema { return schema{"anyOf": []any{v, schema{"type": "null"}}} }
func array(v schema) schema    { return schema{"type": "array", "items": v} }
func object(properties schema, required ...string) schema {
	v := schema{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		v["required"] = required
	}
	return v
}
func wire(t reflect.Type) schema {
	if t == reflect.TypeFor[time.Time]() {
		return schema{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return nullable(wire(t.Elem()))
	case reflect.String:
		return scalar("string")
	case reflect.Bool:
		return scalar("boolean")
	case reflect.Int, reflect.Int64, reflect.Uint64:
		return schema{"type": "integer", "format": "int64"}
	case reflect.Float64:
		return scalar("number")
	case reflect.Slice:
		return nullable(array(wire(t.Elem())))
	case reflect.Map:
		return schema{"type": "object", "additionalProperties": true}
	case reflect.Interface:
		return schema{}
	case reflect.Struct:
		name := t.Name()
		if _, ok := schemas[name]; !ok {
			schemas[name] = schema{}
			properties := schema{}
			required := []string{}
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				parts := strings.Split(f.Tag.Get("json"), ",")
				if parts[0] == "" || parts[0] == "-" {
					continue
				}
				v := wire(f.Type)
				if strings.Contains(f.Tag.Get("json"), ",string") {
					v = schema{"type": "string", "pattern": "^-?[0-9]+$"}
					if f.Type.Kind() == reflect.Pointer {
						v = nullable(v)
					}
				}
				properties[parts[0]] = v
				if !strings.Contains(f.Tag.Get("json"), "omitempty") {
					required = append(required, parts[0])
				}
			}
			schemas[name] = object(properties, required...)
		}
		return ref(name)
	}
	panic(t.String())
}
func model(name string, properties schema, required ...string) {
	schemas[name] = object(properties, required...)
}
func requestFrom(name, base, fields string) {
	props := schemas[base].(schema)["properties"].(schema)
	out := schema{}
	required := []string{}
	for _, field := range strings.Fields(fields) {
		optional := strings.HasSuffix(field, "?")
		field = strings.TrimSuffix(field, "?")
		out[field] = props[field]
		if !optional {
			required = append(required, field)
		}
	}
	model(name, out, required...)
}
func page(name, item string, total bool) {
	p := schema{"items": nullable(array(ref(item)))}
	r := []string{"items"}
	if total {
		p["total"] = scalar("integer")
		r = append(r, "total")
	}
	model(name, p, r...)
}

type route struct {
	method, path, request, response, status, auth, description string
	paged                                                      bool
}

func main() {
	if _, err := os.Stat("go.mod"); err != nil {
		if err = os.Chdir("../.."); err != nil {
			panic(err)
		}
	}
	for _, v := range []any{contract.SiteSettings{}, contract.Exit{}, contract.Diagnostic{}, platform.ImportPreviewItem{}, commerce.PurchaseRefund{}, contract.User{}, contract.UserCreated{}, contract.UserPasswordReset{}, contract.IdentityGroup{}, contract.Group{}, contract.Node{}, contract.Rule{}, contract.Config{}, contract.Registration{}, contract.Registered{}, contract.Ack{}, contract.Probe{}, contract.UsageBatch{}, contract.APIError{}, commerce.Plan{}, commerce.Wallet{}, commerce.Order{}, commerce.Ledger{}, commerce.Entitlement{}, commerce.AutoRenew{}, commerce.RedeemCode{}, commerce.ReferralCode{}, commerce.Commission{}, commerce.Refund{}, commerce.WebhookSubscription{}, commerce.WebhookSettings{}, alerts.Policy{}, commerce.Event{}, platform.Task{}, platform.ProbeHistoryPoint{}, contract.APIToken{}, contract.DeviceIP{}} {
		wire(reflect.TypeOf(v))
	}
	str, num, flag := scalar("string"), scalar("integer"), scalar("boolean")
	money := schema{"type": "string", "pattern": "^-?[0-9]+$"}
	date := schema{"type": "string", "format": "date-time"}
	ids := array(str)
	model("Empty", schema{})
	model("Login", schema{"captcha_id": str, "captcha_answer": str, "username": str, "password": schema{"type": "string", "writeOnly": true}}, "username", "password")
	model("Session", schema{"user": ref("User")}, "user")
	model("PasswordChange", schema{"current_password": str, "password": schema{"type": "string", "minLength": 12, "maxLength": 72}}, "current_password", "password")
	// 有效期二选一：给出 expires_at（一年以内），或声明 permanent。凭据明文
	// 只在创建与重置的响应里出现一次，之后任何接口都取不回来。
	model("TokenCreate", schema{"name": str, "expires_at": date, "permanent": flag}, "name")
	tokenFields := schemas["APIToken"].(schema)["properties"].(schema)
	secret := schema{"token": str, "user_id": str}
	for key, value := range tokenFields {
		secret[key] = value
	}
	model("TokenSecret", secret, "id", "token", "name", "prefix", "scope")
	model("TokenPage", schema{"items": nullable(array(ref("APIToken"))), "total": num}, "items", "total")
	model("DeviceIPView", schema{"device": ref("DeviceIP")}, "device")
	model("DeviceIPPage", schema{"items": nullable(array(ref("DeviceIP"))), "total": num}, "items", "total")
	model("UserCreate", schema{"username": str, "role": schema{"type": "string", "enum": []string{"user", "admin"}}, "identity_group_id": str}, "username", "role")
	model("UserIdentityGroup", schema{"identity_group_id": str}, "identity_group_id")
	identityGroupID := schema{"type": "string", "minLength": 1, "maxLength": 64, "pattern": "^[A-Za-z0-9_-]{1,64}$", "description": "Administrator-specified identity group ID; editable with references updated atomically."}
	model("IdentityGroupCreate", schema{"id": identityGroupID, "name": schema{"type": "string", "minLength": 1, "maxLength": 190}}, "id", "name")
	userCreatedFields := schemas["UserCreated"].(schema)["properties"].(schema)
	userCreatedFields["initial_password"] = schema{
		"type":        "string",
		"pattern":     "^[A-Za-z0-9]{8}(?:-[A-Za-z0-9]{8}){3}$",
		"readOnly":    true,
		"description": "System-generated initial password returned exactly once.",
	}
	passwordResetFields := schemas["UserPasswordReset"].(schema)["properties"].(schema)
	passwordResetFields["password"] = schema{
		"type":        "string",
		"pattern":     "^[A-Za-z0-9]{8}(?:-[A-Za-z0-9]{8}){3}$",
		"readOnly":    true,
		"description": "System-generated replacement password returned exactly once.",
	}
	model("UserStatus", schema{"disabled": flag}, "disabled")
	model("Enrollment", schema{"name": str, "group_ids": schema{"type": "array", "items": str, "minItems": 1, "maxItems": 100}}, "group_ids")
	model("EnrollmentSecret", schema{"token": str, "expires_at": date}, "token", "expires_at")
	model("GroupJoinKey", schema{"group_id": str, "join_key": str}, "group_id", "join_key")
	model("NodeSecret", schema{"node_id": str, "token": str}, "node_id", "token")
	model("Health", schema{"status": str, "database": str, "version": num}, "status", "database", "version")
	model("Audit", schema{"id": str, "user_id": str, "action": str, "target": str, "created_at": date}, "id", "user_id", "action", "target", "created_at")
	model("RetireLease", schema{"lease_id": str, "used_bytes": money}, "lease_id", "used_bytes")
	model("UsageAccepted", schema{"accepted": ids}, "accepted")
	model("DiagnosticCheck", schema{"name": str, "ok": flag, "detail": str}, "name", "ok", "detail")
	model("Diagnostic", schema{"rule_id": str, "node_id": str, "desired_version": num, "applied_version": num, "generated_at": date, "checks": array(ref("DiagnosticCheck"))}, "rule_id", "node_id", "desired_version", "applied_version", "generated_at", "checks")
	groupFields := schemas["Group"].(schema)["properties"].(schema)
	groupFields["type"] = schema{"type": "string", "enum": []string{"", "monitor", "entry", "exit", "chain_exit"}, "description": "Device group role; immutable after creation. Empty preserves legacy groups."}
	groupFields["direct_policy"] = schema{"type": "string", "enum": []string{"", "forbid", "allow", "force"}, "description": "Entry-only direct forwarding policy."}
	groupFields["chain_group_ids"] = schema{"type": "array", "items": str, "minItems": 2, "maxItems": 3, "description": "Ordered physical exit group IDs for a chain_exit group."}
	groupFields["advanced"] = schema{"$ref": "#/components/schemas/GroupAdvanced", "description": "Optional reference-compatible device-group settings; shown separately from the basic group form."}
	advancedSchema := schemas["GroupAdvanced"].(schema)
	// Every input setting is optional; responses still include numeric zeroes.
	delete(advancedSchema, "required")
	advancedFields := advancedSchema["properties"].(schema)
	advancedFields["blocked_protocol"].(schema)["description"] = "Application protocol blacklist: http, socks. Responses use reference names without the legacy app: prefix."
	advancedFields["tls_inbound_policy"] = schema{"type": "integer", "enum": []int{0, 1, 2}, "description": "0: allow ordinary rules; 1: TLS inbound rules only; 2: TLS inbound rules and administrator-owned independent ports only."}
	advancedFields["max_fail"] = schema{"type": "integer", "minimum": 0, "maximum": 1000, "description": "Consecutive failures tolerated before failover for entry-to-exit or direct forwarding. New editor template uses 3; explicit zero is preserved."}
	advancedFields["fail_timout_sec"] = schema{"type": "integer", "minimum": 0, "maximum": 86400, "description": "Failover duration in seconds; reference spelling is intentional. New editor template uses 30; explicit zero is preserved."}
	advancedFields["protocol"] = schema{"type": "string", "enum": []string{"", "tls", "tls_simple", "ws", "http"}, "description": "Reverse tunnel protocol; new editor template uses tls."}
	groupFields["blocked_protocols"].(schema)["description"] = "Application traffic blocks: app:http, app:socks. Independent of forwarding methods. Legacy network:/transport: entries and bare carrier values are accepted on write and split into disabled_networks/disabled_transports; bare http historically means the HTTP tunnel."
	groupFields["disabled_networks"].(schema)["description"] = "Disabled forwarding networks: tcp, udp. Empty means all networks are allowed."
	groupFields["disabled_transports"].(schema)["description"] = "Disabled forwarding methods: direct, direct-tls, tls, ws, wss, http. Empty means all methods are allowed. Applies to every tunnel hop, not application traffic detection."
	requestFrom("GroupCreate", "Group", "name type? direct_policy? chain_group_ids? advanced? identity_group_ids? blocked_protocols? disabled_networks? disabled_transports? multiplier? port_min port_max max_rules?")
	requestFrom("GroupUpdate", "Group", "name type? direct_policy? chain_group_ids? advanced? identity_group_ids? blocked_protocols? disabled_networks? disabled_transports? multiplier? port_min port_max max_rules? version")
	requestFrom("RuleCreate", "Rule", "user_id? name node_id group_id network transport listen target enabled blocked_protocols? tunnel? backends? shared_tls? proxy_protocol? exit_group_id? exit_id?")
	requestFrom("RuleUpdate", "Rule", "user_id? name node_id group_id network transport listen target enabled blocked_protocols? tunnel? backends? shared_tls? proxy_protocol? exit_group_id? exit_id? version")
	requestFrom("PlanCreate", "Plan", "name price_cents quota_bytes months kind? limits?")
	requestFrom("PlanUpdate", "Plan", "name price_cents quota_bytes months kind version limits?")
	model("PlanActive", schema{"active": flag}, "active")
	model("PlanActiveResult", schema{"id": str, "active": flag}, "id", "active")
	model("OrderCreate", schema{"channel": schema{"type": "string", "enum": []string{"epay", "epusdt", "bepusdt", "tokenpay", "cryptomus"}}, "amount_cents": money, "idempotency_key": str}, "channel", "amount_cents", "idempotency_key")
	model("Purchase", schema{"plan_id": str, "expected_version": num, "expected_plan_version": num, "idempotency_key": str}, "plan_id", "expected_version", "idempotency_key")
	model("PurchaseSnapshot", schema{"entitlement_id": str, "plan": ref("Plan"), "created_at": date}, "entitlement_id", "plan", "created_at")
	model("AutoRenewSet", schema{"enabled": flag, "plan_id": str}, "enabled")
	model("RefundRequest", schema{"amount_cents": money, "idempotency_key": str, "reason": str}, "amount_cents", "idempotency_key", "reason")
	model("RefundResolution", schema{"completed": flag, "evidence": str}, "completed", "evidence")
	model("RedeemCreate", schema{"amount_cents": money, "plan_id": str, "max_uses": schema{"type": "integer", "minimum": 1, "maximum": 1000000}, "expires_at": date}, "max_uses")
	model("RedeemSecret", schema{"id": str, "code": str, "code_hint": str, "amount_cents": money, "plan_id": str, "max_uses": num, "expires_at": nullable(date)}, "id", "code", "code_hint", "amount_cents", "plan_id", "max_uses", "expires_at")
	model("CodeInput", schema{"code": str}, "code")
	model("RedeemResult", schema{"amount_cents": money, "plan_id": str, "entitlement_id": str}, "amount_cents", "plan_id")
	model("ReferralSecret", schema{"code": str, "code_hint": str, "created_at": date}, "code", "code_hint", "created_at")
	model("ReferralBound", schema{"bound": flag}, "bound")
	model("CommissionPolicy", schema{"rate_bps": schema{"type": "integer", "minimum": 0, "maximum": 10000}}, "rate_bps")
	model("CommissionResolution", schema{"action": schema{"type": "string", "enum": []string{"settle", "reverse"}}, "reason": str}, "action", "reason")
	model("WebhookCreate", schema{"format": schema{"type": "string", "enum": []string{"webhook", "feishu", "discord"}}, "url": schema{"type": "string", "format": "uri", "maxLength": 2048}, "events": array(str)}, "url", "events")
	model("WebhookSecret", schema{"id": str, "url": str, "events": array(str), "secret": str, "created_at": date}, "id", "url", "events", "secret", "created_at")
	model("WebhookDelivery", schema{"event_id": str, "subscription_id": str, "attempts": num, "status": schema{"type": "string", "enum": []string{"pending", "failed", "delivered", "suppressed", "access_revoked"}}, "next_at": date, "last_error": str, "delivered_at": str}, "event_id", "subscription_id", "attempts", "status", "next_at", "last_error", "delivered_at")
	model("PaymentChannel", schema{"id": str, "name": str, "enabled": flag, "status": str, "reason": str}, "id", "name", "enabled", "status", "reason")
	model("ExportTask", schema{"rule_ids": schema{"type": "array", "items": str, "maxItems": 500}, "idempotency_key": schema{"type": "string", "minLength": 1, "maxLength": 128}}, "idempotency_key")
	model("ImportTask", schema{"mode": schema{"type": "string", "enum": []string{"create", "update_by_port"}}, "rules": schema{"type": "array", "items": ref("Rule"), "minItems": 1, "maxItems": 1000}, "idempotency_key": schema{"type": "string", "minLength": 1, "maxLength": 128}}, "rules", "idempotency_key")
	model("TaskCancel", schema{"status": schema{"const": "cancel_requested"}}, "status")
	for _, name := range []string{"User", "IdentityGroup", "Group", "Node", "Rule", "Audit", "Plan", "Order", "Ledger", "ProbeHistoryPoint", "PaymentChannel"} {
		page(name+"Page", name, true)
	}
	for _, name := range []string{"Probe", "Task", "Refund", "RedeemCode", "Commission", "WebhookSubscription", "WebhookDelivery", "Event", "PurchaseSnapshot"} {
		page(name+"List", name, false)
	}
	schemas["EntitlementOrNull"] = nullable(ref("Entitlement"))
	schemas["Null"] = scalar("null")
	schemas["PaymentNotify"] = schema{"description": "Provider-specific signed payload. See docs/payment/protocol-sources.md; values are validated by the configured protocol adapter.", "type": "object", "additionalProperties": true}
	model("Upgrade", schema{"url": schema{"type": "string", "format": "uri"}, "sha256": str, "signature": str, "version": str, "os": str, "arch": str}, "url", "sha256", "signature", "version", "os", "arch")
	// WebSSH 不带密码：只给 scope=shell 就能换到开终端的授权；卸载与升级仍然要
	// password，换来的授权是 sensitive。两者给一个即可，所以都不设 required。
	schemas["OperationAccess"] = schema{
		"type":                 "object",
		"additionalProperties": false,
		"description":          "Exactly one of scope=shell (WebSSH, no password) or password (full grant, required by uninstall and upgrade).",
		"properties": schema{
			"password": schema{"type": "string", "writeOnly": true},
			"scope":    schema{"type": "string", "enum": []any{"shell", "sensitive"}},
		},
	}
	model("OperationAccessSecret", schema{"token": str, "expires_at": date}, "token", "expires_at")
	model("OperationCreate", schema{"access_token": str, "idempotency_key": str}, "access_token", "idempotency_key")
	model("UpgradeOperationCreate", schema{"access_token": str, "idempotency_key": str, "upgrade": ref("Upgrade")}, "access_token", "idempotency_key", "upgrade")
	model("NodeOperation", schema{"id": str, "node_id": str, "kind": str, "status": str, "claim": str, "upgrade": nullable(ref("Upgrade")), "error": str, "created_at": date, "expires_at": date}, "id", "node_id", "kind", "status", "created_at", "expires_at")
	model("LookingGlass", schema{"id": str, "node_id": str, "method": str, "target": str, "status": str, "output": str, "error": str, "created_at": date}, "id", "node_id", "method", "target", "status", "created_at")
	model("LookingGlassInput", schema{"method": str, "target": str}, "method", "target")
	model("LookingGlassDispatch", schema{"request": nullable(ref("LookingGlass"))})
	model("NodeOperationList", schema{"items": array(ref("NodeOperation"))}, "items")
	model("Control", schema{"operation": nullable(ref("NodeOperation")), "active": array(str)}, "active")
	model("OperationResult", schema{"id": str, "claim": str, "status": str, "error": str}, "id", "claim", "status", "error")
	model("TerminalCommand", schema{"type": str, "command": str, "data": str, "code": num}, "type")
	model("CommandAudit", schema{"command": str, "created_at": date}, "command", "created_at")
	model("CommandAuditList", schema{"items": array(ref("CommandAudit"))}, "items")
	schemas["OpenAPIDocument"] = schema{"type": "object", "additionalProperties": true}
	page("ExitPage", "Exit", true)
	page("FundingList", "Funding", false)
	model("Funding", schema{"order_id": str, "amount_cents": money, "refunded_cents": money}, "order_id", "amount_cents", "refunded_cents")
	model("PurchaseRefundInput", schema{"amount_cents": money, "reason": str, "idempotency_key": str}, "amount_cents", "reason", "idempotency_key")
	model("Captcha", schema{"id": str, "image": str}, "id", "image")
	model("RegistrationInput", schema{"username": str, "password": str, "invite": str, "captcha_id": str, "captcha_answer": str}, "username", "password")
	model("RegistrationInvite", schema{"code": str}, "code")
	model("DiagnosticDispatch", schema{"diagnostic": nullable(ref("Diagnostic"))}, "diagnostic")
	model("ImportPreviewInput", schema{"mode": str, "rules": array(ref("Rule"))}, "rules")
	model("ImportPreview", schema{"mode": str, "items": array(ref("ImportPreviewItem"))}, "mode", "items")
	routes := []route{
		{"GET", "/site", "", "SiteSettings", "200", "public", "Public site settings and registration policy", false},
		{"PUT", "/site", "SiteSettings", "SiteSettings", "200", "admin", "Versioned site settings update", false},
		{"GET", "/auth/captcha", "", "Captcha", "200", "public", "Single-use image challenge valid for three minutes", false},
		{"POST", "/auth/register", "RegistrationInput", "User", "201", "public", "Register under current site policy", false},
		{"POST", "/registration-invites", "Empty", "RegistrationInvite", "201", "admin", "Issue single-use registration invitation", false},
		{"GET", "/exits", "", "ExitPage", "200", "user", "Authorized exit directory; credentials redacted", true},
		{"POST", "/exits", "Exit", "Exit", "201", "admin", "Register a managed exit", false},
		{"PUT", "/exits/{id}", "Exit", "Exit", "200", "admin", "Update managed exit with version check", false},
		{"POST", "/tasks/rules/preview", "ImportPreviewInput", "ImportPreview", "200", "user", "Transactional dry run; no resource or money changes persist", false},
		{"POST", "/rules/{id}/network-diagnostic", "Empty", "Diagnostic", "202", "user", "Queue an authorized TCP rule network diagnostic", false},
		{"GET", "/diagnostics/{id}", "", "Diagnostic", "200", "user", "Read sanitized diagnostic result with current authorization", false},
		{"POST", "/agent/diagnostics", "", "DiagnosticDispatch", "200", "agent", "Claim one bounded diagnostic task", false},
		{"POST", "/agent/diagnostics/result", "Diagnostic", "", "204", "agent", "Submit fenced diagnostic result", false},
		{"POST", "/agent/looking-glass", "", "LookingGlassDispatch", "200", "agent", "Claim one pending looking glass request", false},
		{"POST", "/agent/looking-glass/result", "LookingGlass", "", "204", "agent", "Submit one fenced looking glass result", false},
		{"GET", "/purchases/{id}/funding", "", "FundingList", "200", "user", "Read original funding and returned amounts", false},
		{"POST", "/purchases/{id}/refund", "PurchaseRefundInput", "PurchaseRefund", "200", "admin", "Refund unused purchased quota to original wallet funds and reverse commissions", false},
		{"GET", "/openapi.json", "", "OpenAPIDocument", "200", "public", "OpenAPI 3.1 document", false},
		{"POST", "/auth/login", "Login", "Session", "200", "public", "Cookie login; rate limited", false},
		{"GET", "/auth/session", "", "Session", "200", "user", "Current account", false},
		{"POST", "/auth/logout", "Empty", "", "204", "user", "End cookie session", false},
		{"POST", "/auth/password", "PasswordChange", "", "204", "user", "Change password and revoke sessions/tokens", false},
		{"GET", "/auth/tokens", "", "TokenPage", "200", "user", "List own API tokens; no secrets", true},
		{"POST", "/auth/tokens", "TokenCreate", "TokenSecret", "201", "user", "Issue owner-resources token; expiry within one year", false},
		{"DELETE", "/auth/tokens/{id}", "", "", "204", "user", "Revoke own token", false},
		{"GET", "/users/{id}/tokens", "", "TokenPage", "200", "admin", "List one account's API tokens with prefixes and last use; never secrets", true},
		{"POST", "/users/{id}/tokens", "TokenCreate", "TokenSecret", "201", "admin", "Issue an API token for an account; the secret is returned exactly once", false},
		{"POST", "/users/{id}/tokens/{token_id}/reset", "Empty", "TokenSecret", "200", "admin", "Replace a token secret in place; the new secret is returned exactly once", false},
		{"DELETE", "/users/{id}/tokens/{token_id}", "", "", "204", "admin", "Revoke an account's API token", false},
		{"GET", "/online/device/ip", "", "DeviceIPView", "200", "user", "Latest address of the single device visible to this API token; 409 when the group has several", false},
		{"GET", "/online/device/ip/list", "", "DeviceIPPage", "200", "user", "Latest addresses of every device visible to this API token, grouped and ordered", false},
		{"GET", "/health", "", "Health", "200", "public", "Health and database kind", false},
		{"GET", "/users", "", "UserPage", "200", "admin", "List users", true},
		{"POST", "/users", "UserCreate", "UserCreated", "201", "admin", "Create user and return the system-generated initial password exactly once", false},
		{"PUT", "/users/{id}/identity-group", "UserIdentityGroup", "", "204", "admin", "Assign a user to an identity group", false},
		{"POST", "/users/{id}/reset-password", "Empty", "UserPasswordReset", "200", "admin", "Reset a user password, revoke sessions and tokens, and return the replacement exactly once", false},
		{"PUT", "/users/{id}/status", "UserStatus", "", "204", "admin", "Disable/enable non-administrator; revoke sessions", false},
		{"GET", "/identity-groups", "", "IdentityGroupPage", "200", "admin", "List identity groups and reference counts", true},
		{"POST", "/identity-groups", "IdentityGroupCreate", "IdentityGroup", "201", "admin", "Create an identity group", false},
		{"PUT", "/identity-groups/{id}", "IdentityGroupCreate", "IdentityGroup", "200", "admin", "Edit identity group ID and name while preserving user and device-group references", false},
		{"DELETE", "/identity-groups/{id}", "", "", "204", "admin", "Delete an unreferenced identity group", false},
		{"GET", "/groups", "", "GroupPage", "200", "user", "Device groups authorized through the current user's identity group", true},
		{"POST", "/groups", "GroupCreate", "Group", "201", "admin", "Create device group", false},
		{"PUT", "/groups/{id}", "GroupUpdate", "Group", "200", "admin", "Update group with version check", false},
		{"DELETE", "/groups/{id}", "", "", "204", "admin", "Delete an unused device group at the expected version; detach devices and grants, remove idle exits; 409 while rules or other groups reference it or removal awaits Agent ACK; 404 if missing. Devices, leases and usage history are retained", false},
		{"GET", "/groups/{id}/join-key", "", "GroupJoinKey", "200", "admin", "Fixed per-group access key behind the device onboarding command; readable again at any time", false},
		{"POST", "/groups/{id}/join-key", "Empty", "GroupJoinKey", "200", "admin", "Rotate the group access key; commands already distributed stop working", false},
		{"GET", "/nodes", "", "NodePage", "200", "user", "Authorized nodes", true},
		{"POST", "/nodes/enrollment", "Enrollment", "EnrollmentSecret", "201", "admin", "One-time node enrollment valid for 15 minutes", false},
		{"POST", "/nodes/{id}/rotate-token", "Empty", "NodeSecret", "200", "admin", "Rotate node credential", false},
		{"POST", "/nodes/{id}/operation-access", "OperationAccess", "OperationAccessSecret", "201", "admin", "Short-lived node-operation grant; WebSSH asks for scope=shell alone, uninstall and upgrade require the password", false},
		{"POST", "/nodes/{id}/terminal", "OperationCreate", "NodeOperation", "201", "admin", "Create an audited remote terminal task", false},
		{"POST", "/nodes/{id}/shell", "OperationCreate", "NodeOperation", "201", "admin", "Create an interactive PTY terminal task", false},
		{"POST", "/nodes/{id}/uninstall", "OperationCreate", "NodeOperation", "201", "admin", "Create a managed Agent uninstall task", false},
		{"POST", "/nodes/{id}/looking-glass", "LookingGlassInput", "LookingGlass", "202", "admin", "Run ping, tcping or mtr from the node; argv is built server-side and never goes through a shell", false},
		{"GET", "/looking-glass/{id}", "", "LookingGlass", "200", "user", "Poll one looking glass result", false},
		{"POST", "/nodes/{id}/upgrade", "UpgradeOperationCreate", "NodeOperation", "201", "admin", "Create a signed Agent upgrade task", false},
		{"GET", "/nodes/{id}/operations", "", "NodeOperationList", "200", "admin", "List node operation status", false},
		{"POST", "/node-operations/{id}/cancel", "Empty", "", "204", "admin", "Cancel a pending node operation", false},
		{"GET", "/node-operations/{id}/terminal", "", "", "101", "admin", "Authenticated browser terminal WebSocket", false},
		{"GET", "/node-operations/{id}/commands", "", "CommandAuditList", "200", "admin", "List terminal command audit records", false},
		{"GET", "/rules", "", "RulePage", "200", "user", "Own rules; admin sees all. Credentials and lease removed", true},
		{"POST", "/rules", "RuleCreate", "Rule", "201", "user", "Create rule, reserve port and finite allowance", false},
		{"PUT", "/rules/{id}", "RuleUpdate", "Rule", "200", "user", "Update rule with optimistic version; placement immutable", false},
		{"DELETE", "/rules/{id}", "", "", "204", "user", "Delete at expected version; port held until Agent ACK", false},
		{"GET", "/rules/{id}/diagnose", "", "Diagnostic", "200", "user", "Control-plane status checks; does not probe network targets", false},
		{"GET", "/probes", "", "ProbeList", "200", "user", "Authorized probes; ordinary users never receive public_ips", false},
		{"GET", "/probes/events", "", "ProbeList", "200", "user", "SSE event probes; recheck identity/access every 5 seconds", false},
		{"GET", "/probes/{node_id}/history", "", "ProbeHistoryPointPage", "200", "user", "UTC range [from,to), minute 7 days/hour 180 days, absent metrics null", false},
		{"GET", "/audit", "", "AuditPage", "200", "admin", "Audit records", true},
		{"POST", "/tasks/rules/export", "ExportTask", "Task", "202", "user", "Export up to 10000 rules; credentials redacted", false},
		{"POST", "/tasks/rules/import", "ImportTask", "Task", "202", "user", "Create rules with durable per-item checkpoints; 1 MiB request limit", false},
		{"GET", "/tasks", "", "TaskList", "200", "user", "Own last 100 tasks; privileged tasks require current admin role", false},
		{"GET", "/tasks/{id}", "", "Task", "200", "user", "Task result JSON string; kept seven days", false},
		{"POST", "/tasks/{id}/cancel", "Empty", "TaskCancel", "202", "user", "Cancel pending work; completed import items remain", false},
		{"POST", "/agent/register", "Registration", "Registered", "201", "public", "Consume one-time enrollment token", false},
		{"GET", "/agent/config", "", "Config", "200", "node", "Finite authorized configuration including tunnel secrets", false},
		{"POST", "/agent/ack", "Ack", "", "204", "node", "Versioned configuration acknowledgement", false},
		{"POST", "/agent/probe", "Probe", "", "204", "node", "Submit actual probe observations", false},
		{"POST", "/agent/usage", "UsageBatch", "UsageAccepted", "200", "node", "At most 500 usage facts; acknowledge only after durable settlement", false},
		{"POST", "/agent/leases/retire", "RetireLease", "", "204", "node", "Retire lease only after final usage is settled", false},
		{"POST", "/agent/control", "", "Control", "200", "node", "Claim one authorized node operation", false},
		{"POST", "/agent/control/result", "OperationResult", "", "204", "node", "Report operation completion", false},
		{"GET", "/agent/control/{id}/terminal", "", "", "101", "node", "Agent-side terminal WebSocket", false},
		{"GET", "/plans", "", "PlanPage", "200", "user", "Plans including off-sale plans", true},
		{"POST", "/plans", "PlanCreate", "Plan", "200", "admin", "Create period plan or traffic add-on", false},
		{"PUT", "/plans/{id}", "PlanUpdate", "Plan", "200", "admin", "Versioned edit; sold snapshots remain immutable", false},
		{"PATCH", "/plans/{id}", "PlanActive", "PlanActiveResult", "200", "admin", "Set sale availability and increment version", false},
		{"GET", "/wallet", "", "Wallet", "200", "user", "Available CNY balance in integer cents", false},
		{"GET", "/ledger", "", "LedgerPage", "200", "user", "Own immutable wallet ledger", true},
		{"GET", "/orders", "", "OrderPage", "200", "user", "Own recharge orders", true},
		{"POST", "/orders", "OrderCreate", "Order", "200", "user", "Create/replay recharge intent; payment_uncertain retains original key", false},
		{"POST", "/orders/{id}/reconcile", "Empty", "Order", "200", "user", "Query verified provider status; unsupported protocols return 422", false},
		{"POST", "/orders/{id}/close", "Empty", "Order", "200", "user", "Close locally; valid late receipt still credits wallet", false},
		{"POST", "/orders/{id}/refund", "RefundRequest", "Refund", "200", "admin", "Reserve wallet funds for manual external refund", false},
		{"GET", "/orders/{id}/refunds", "", "RefundList", "200", "user", "Owner or admin; owner cannot read internal evidence or actor ids", false},
		{"POST", "/refunds/{id}/resolve", "RefundResolution", "Refund", "200", "admin", "Record external transfer evidence or cancel reservation", false},
		{"GET", "/entitlement", "", "EntitlementOrNull", "200", "user", "Current period; null when never purchased", false},
		{"POST", "/purchases", "Purchase", "Entitlement", "200", "user", "Wallet purchase resets period immediately; quote version optional", false},
		{"GET", "/purchases", "", "PurchaseSnapshotList", "200", "user", "Last 100 period purchase snapshots since schema v3", false},
		{"POST", "/addon-purchases", "Purchase", "Entitlement", "200", "user", "Add current quota without changing expiry; exact replay", false},
		{"GET", "/auto-renew", "", "AutoRenew", "200", "user", "Auto-renew setting and last failure code", false},
		{"POST", "/auto-renew", "AutoRenewSet", "AutoRenew", "200", "user", "Default off; purchase only after expiry, retry hourly on failure", false},
		{"GET", "/redeem-codes", "", "RedeemCodeList", "200", "admin", "Last 100 code metadata records; no plaintext", false},
		{"POST", "/redeem-codes", "RedeemCreate", "RedeemSecret", "200", "admin", "Issue wallet and/or period grant code; plaintext once", false},
		{"DELETE", "/redeem-codes/{id}", "", "Null", "200", "admin", "Revoke code; existing claims retained", false},
		{"POST", "/redeem", "CodeInput", "RedeemResult", "200", "user", "Redeem once per account; repeat returns original result", false},
		{"POST", "/referrals", "Empty", "ReferralSecret", "200", "user", "Create one invitation per account; plaintext once", false},
		{"POST", "/referrals/bind", "CodeInput", "ReferralBound", "200", "user", "Permanent binding before first purchase; cycles rejected", false},
		{"GET", "/commissions", "", "CommissionList", "200", "user", "Own last 100 commissions; initially pending", false},
		{"GET", "/commission-policy", "", "CommissionPolicy", "200", "admin", "Configured basis point rate; default zero", false},
		{"PUT", "/commission-policy", "CommissionPolicy", "CommissionPolicy", "200", "admin", "Set rate for subsequent period purchases only", false},
		{"POST", "/commissions/{id}/resolve", "CommissionResolution", "Null", "200", "admin", "Audited settlement/reversal; reversal needs available funds", false},
		{"GET", "/alert-policy", "", "Policy", "200", "user", "Alert thresholds and debounce policy", false},
		{"PUT", "/alert-policy", "Policy", "Policy", "200", "admin", "Update versioned alert policy", false},
		{"PUT", "/webhooks/{id}", "WebhookSettings", "Null", "200", "user", "Change events, enablement or mute until (up to 30 days); suppressed events are not replayed", false},
		{"GET", "/webhooks", "", "WebhookSubscriptionList", "200", "user", "Own subscriptions; secret hidden", false},
		{"POST", "/webhooks", "WebhookCreate", "WebhookSecret", "200", "user", "Up to 8 public HTTPS endpoints; secret shown once", false},
		{"DELETE", "/webhooks/{id}", "", "Null", "200", "user", "Delete own subscription and queued deliveries", false},
		{"GET", "/webhook-deliveries", "", "WebhookDeliveryList", "200", "user", "Last 100 attempts; at most 12 tries per delivery", false},
		{"GET", "/events", "", "EventList", "200", "user", "Own events; 30 day retention, signed payload preserved", false},
		{"GET", "/payment-channels", "", "PaymentChannelPage", "200", "user", "Five supported payment protocols; Cyber excluded", false},
		{"GET", "/payments/epay/notify", "", "", "200", "provider", "EPay signed query parameters; literal provider acknowledgement", false},
		{"POST", "/payments/epay/notify", "PaymentNotify", "", "200", "provider", "EPay signed form callback", false},
		{"POST", "/payments/{channel}/notify", "PaymentNotify", "", "200", "provider", "Configured provider signed callback; literal provider acknowledgement", false},
	}
	paths := schema{}
	for _, r := range routes {
		operation := schema{"operationId": strings.ToLower(r.method) + strings.NewReplacer("/", "_", "{", "", "}", "", ".", "_").Replace(r.path), "summary": r.description, "x-role": r.auth}
		responses := schema{}
		success := schema{"description": "Success"}
		if r.response != "" {
			success["content"] = schema{"application/json": schema{"schema": ref(r.response)}}
		}
		responses[r.status] = success
		if r.auth == "provider" {
			success["content"] = schema{"text/plain": schema{"schema": scalar("string")}}
		} else if r.path == "/probes/events" {
			success["content"] = schema{"text/event-stream": schema{"schema": scalar("string"), "example": "event: probes\ndata: {\"items\":[]}\n\n"}}
		}
		responses["default"] = schema{"description": "Failure; some auth failures are plain text. 409 state conflict/payment_uncertain, 422 unsupported operation.", "content": schema{"application/json": schema{"schema": ref("APIError")}, "text/plain": schema{"schema": scalar("string")}}}
		operation["responses"] = responses
		switch r.auth {
		case "public", "provider":
			operation["security"] = []any{}
		case "admin":
			operation["security"] = []any{schema{"cookieSession": []string{}}}
		case "node", "agent":
			operation["security"] = []any{schema{"nodeBearer": []string{}}}
		default:
			operation["security"] = []any{schema{"cookieSession": []string{}}, schema{"ownerBearer": []string{}}}
		}
		params := []any{}
		for _, part := range strings.Split(r.path, "/") {
			if strings.HasPrefix(part, "{") {
				params = append(params, schema{"name": strings.Trim(part, "{}"), "in": "path", "required": true, "schema": scalar("string")})
			}
		}
		if r.paged {
			for _, name := range []string{"page", "page_size"} {
				params = append(params, schema{"name": name, "in": "query", "schema": schema{"type": "integer", "minimum": 1}, "description": "page starts at 1; page_size defaults to 20, capped at 100"})
			}
		}
		if r.method != "GET" && r.auth != "public" && r.auth != "provider" && r.auth != "node" && r.auth != "agent" {
			params = append(params, schema{"name": "X-Requested-With", "in": "header", "schema": schema{"const": "fetch"}, "description": "Required with Cookie mutations, together with matching Origin; Bearer requests exempt"})
		}
		if r.path == "/probes/{node_id}/history" {
			for _, name := range []string{"resolution", "from", "to"} {
				params = append(params, schema{"name": name, "in": "query", "schema": scalar("string"), "description": "resolution minute|hour; from/to UTC RFC3339"})
			}
		}
		if r.path == "/events" {
			params = append(params, schema{"name": "limit", "in": "query", "schema": schema{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}})
		}
		if r.path == "/online/device/ip" || r.path == "/online/device/ip/list" {
			operation["x-aliases"] = []string{r.path}
		}
		if r.method == "DELETE" && (r.path == "/rules/{id}" || r.path == "/groups/{id}") {
			params = append(params, schema{"name": "version", "in": "query", "required": true, "schema": num})
		}
		if len(params) > 0 {
			operation["parameters"] = params
		}
		if r.request != "" {
			content := schema{"application/json": schema{"schema": ref(r.request)}}
			if r.auth == "provider" {
				content["application/x-www-form-urlencoded"] = schema{"schema": ref("PaymentNotify")}
			}
			operation["requestBody"] = schema{"required": r.request != "Empty", "content": content}
		}
		if paths[r.path] == nil {
			paths[r.path] = schema{}
		}
		paths[r.path].(schema)[strings.ToLower(r.method)] = operation
	}
	document := schema{"openapi": "3.1.0", "info": schema{"title": "Traffic Forwarding Panel API", "version": "1.0.0", "description": "CNY cents and cumulative byte counts are decimal strings; times are UTC RFC3339. Browser rendering uses Asia/Shanghai. Cookie mutations require X-Requested-With: fetch and matching Origin. Owner Bearer tokens never carry admin privileges. See docs/api-contract.md for transaction, retention and retry semantics."}, "servers": []any{schema{"url": "/api/v1"}}, "paths": paths, "components": schema{"schemas": schemas, "securitySchemes": schema{"cookieSession": schema{"type": "apiKey", "in": "cookie", "name": "tfp_session"}, "ownerBearer": schema{"type": "http", "scheme": "bearer", "description": "Owner resources only, including tokens issued by an administrator"}, "nodeBearer": schema{"type": "http", "scheme": "bearer", "description": "Separate Agent identity"}}}}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile("internal/openapi/openapi.json", append(raw, '\n'), 0644); err != nil {
		panic(err)
	}
	fmt.Println("OpenAPI operations:", len(routes))
}
