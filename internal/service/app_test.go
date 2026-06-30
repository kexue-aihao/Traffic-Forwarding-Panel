package service

import (
	"encoding/json"
	"testing"

	"trafficpanel/internal/domain"
)

func TestNodeCommandPayloadMarshalsSpecialCharacters(t *testing.T) {
	payload := domain.NodeCommandPayload{
		Service: domain.ForwardService{
			ServiceKey: "svc-1",
			Protocol:   domain.ProtocolTCP,
			ListenAddr: "127.0.0.1:9000",
			TargetAddr: "example.com:443\"quoted",
			Status:     domain.ServicePaused,
		},
		Reason: "quota exceeded \"quoted\"",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var decoded domain.NodeCommandPayload
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload should stay valid JSON: %v", err)
	}
	if decoded.Service.TargetAddr != payload.Service.TargetAddr || decoded.Reason != payload.Reason {
		t.Fatalf("unexpected decoded payload: %#v", decoded)
	}
}
