package detect_test

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/association"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

type peerCapture struct {
	wireCapture
	peer netip.AddrPort
}

func (p *peerCapture) RemoteAddr() net.Addr { return net.TCPAddrFromAddrPort(p.peer) }

func TestAssociatedPlanRequiresLiveScopedProof(t *testing.T) {
	profiles := detect.Profiles{"association": {Protocol: "socks5", UDPMode: "associated", RuleIDs: []string{"udp"}, ControlRuleIDs: []string{"control"}, Targets: []string{"127.0.0.1:7000"}, RelayEndpoint: "127.0.0.1:6000"}}
	layers := []contract.InboundPolicy{{BlockedApps: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"association"}}}}
	if _, e := detect.Prepare(layers, profiles, detect.Scope{RuleID: "udp", Target: "127.0.0.1:7000", Network: "udp"}); e == nil {
		t.Fatal("exit without actual source tuple claimed association support")
	}
	plan, e := detect.Prepare(layers, profiles, detect.Scope{RuleID: "udp", Target: "127.0.0.1:7000", Network: "udp", SOCKSAssociation: true, Generation: "policy-and-profile-epoch"})
	if e != nil {
		t.Fatal(e)
	}
	bindings := plan.AssociationBindings()
	if !plan.HasAssociation() || len(bindings) != 1 || bindings[0].ControlRuleID != "control" || bindings[0].Generation != plan.Generation() {
		t.Fatal("local authorized relationship lost")
	}
	packet := []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 'd'}
	d := plan.Feed(packet, true, "udp")
	if d.Status == detect.Match || plan.Decision(d) != nil {
		t.Fatal("preparation capability bool fabricated per-flow association")
	}
	r := association.New(16, time.Minute)
	if e = r.Configure(bindings); e != nil {
		t.Fatal(e)
	}
	client := &peerCapture{peer: netip.MustParseAddrPort("127.0.0.1:30000")}
	target := &peerCapture{peer: netip.MustParseAddrPort("127.0.0.1:7001")}
	wc, wt, cleanup, e := r.Observe(client, target, bindings...)
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	wt.Write([]byte{5, 1, 0})
	wc.Write([]byte{5, 0})
	wt.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	wc.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0x17, 0x70})
	proof := r.Admission("udp", plan.Generation(), netip.MustParseAddrPort("127.0.0.1:31000"), "127.0.0.1:7000", packet)
	d = plan.FeedAssociated(packet, true, "udp", proof)
	if d.Status != detect.Match || d.Variant != "socks5-udp-associated" || !errors.Is(plan.Decision(d), detect.ErrDenied) {
		t.Fatalf("live association failed protocol disable: %+v", d)
	}
	if d.Evidence == detect.Authenticated {
		t.Fatal("SOCKS UDP association falsely claimed encrypted packet authentication")
	}
	r.RevokeRule("control")
	d = plan.FeedAssociated(packet, true, "udp", proof)
	if d.Status == detect.Match || plan.Decision(d) != nil {
		t.Fatal("revoked control proof retained protocol confirmation")
	}
	strictUnknown := *layers[0].Inspection
	strictUnknown.Unknown = "deny"
	layers[0].Inspection = &strictUnknown
	deny, e := detect.Prepare(layers, profiles, detect.Scope{RuleID: "udp", Target: "127.0.0.1:7000", Network: "udp", SOCKSAssociation: true})
	if e != nil {
		t.Fatal(e)
	}
	if !errors.Is(deny.Decision(deny.FeedAssociated(packet, true, "udp", association.Proof{})), detect.ErrDenied) {
		t.Fatal("independent unknown policy ignored")
	}
}
