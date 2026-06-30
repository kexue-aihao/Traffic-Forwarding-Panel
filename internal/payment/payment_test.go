package payment

import (
	"net/url"
	"testing"
)

func TestAmountCentsFromForm(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		want int64
	}{
		{name: "amount cents", form: url.Values{"amount_cents": {"1234"}}, want: 1234},
		{name: "money decimal", form: url.Values{"money": {"12.34"}}, want: 1234},
		{name: "amount decimal", form: url.Values{"amount": {"10"}}, want: 1000},
		{name: "missing", form: url.Values{}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := amountCentsFromForm(tc.form); got != tc.want {
				t.Fatalf("amountCentsFromForm() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSignedFormProviderRequiresSignatureWhenConfigured(t *testing.T) {
	provider := NewSignedFormProvider("epay", "Epay", "https://pay.example", "pid", "secret", "alipay")
	_, err := provider.ParseNotify(t.Context(), nil, url.Values{"out_trade_no": {"ORDER"}, "trade_status": {"TRADE_SUCCESS"}})
	if err == nil {
		t.Fatal("expected missing signature error")
	}
}
