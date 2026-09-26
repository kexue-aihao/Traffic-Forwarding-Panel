package commerce

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

// noQueryAdapter 模拟原版 EPUSDT：能建单、能收回调，但没有查单能力。
type noQueryAdapter struct{ countingAdapter }

func (a *noQueryAdapter) Capabilities() payment.Capabilities {
	return payment.Capabilities{Create: true, Notify: true, NotifyAck: "ok"}
}

// 接口在、功能不在时的口径：该协议没有查单能力是服务端缺实现，按 500 报，
// 不能让调用方以为改改参数就能成功。客户端按 not_implemented 这个码给出
// 「不支持主动查单」的提示。
func TestReconcileWithoutQueryCapabilityIsNotImplemented(t *testing.T) {
	s := fixture(t)
	channel := Channel{Adapter: &noQueryAdapter{}, NotifyURL: "https://panel.invalid/notify", ReturnURL: "https://panel.invalid/"}
	order, e := s.CreateAdapterOrder(context.Background(), "alice", "epusdt", "topup", 1000, channel, "127.0.0.1")
	if e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	s.Register(mux, HTTPOptions{
		Authenticate: func(*http.Request) (contract.User, error) {
			return contract.User{ID: "alice", Role: "user"}, nil
		},
		Channels: map[string]Channel{"epusdt": channel},
	})
	r := httptest.NewRequest("POST", "http://example.test/api/v1/orders/"+order.ID+"/reconcile", strings.NewReader(""))
	r.Header.Set("Origin", "http://example.test")
	r.Header.Set("X-Requested-With", "fetch")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("没有查单能力却按 %d 回了: %s", w.Code, w.Body.String())
	}
	var body contract.APIError
	if e = json.Unmarshal(w.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	if body.Code != "not_implemented" {
		t.Fatalf("错误码不对: %s", w.Body.String())
	}
}
