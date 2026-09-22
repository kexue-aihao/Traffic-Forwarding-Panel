package commerce

import (
	"context"
	"testing"
)

func TestChannelQuoteAddsFeeOnTop(t *testing.T) {
	cases := []struct {
		feeBPS   int
		feeFixed int64
		amount   int64
		payable  int64
		fee      int64
	}{
		{feeBPS: 0, amount: 10000, payable: 10000, fee: 0},
		{feeBPS: 150, amount: 10000, payable: 10150, fee: 150},
		{feeBPS: 150, feeFixed: 100, amount: 10000, payable: 10250, fee: 250},
		// 0.5 分的手续费向下取整到 0 分：宁可少收，也不要浮点凑整。
		{feeBPS: 1, amount: 1, payable: 1, fee: 0},
		{feeBPS: 10000, amount: 10000, payable: 20000, fee: 10000},
	}
	for _, c := range cases {
		channel := Channel{FeeBPS: c.feeBPS, FeeFixed: c.feeFixed}
		payable, fee := channel.Quote(c.amount)
		if payable != c.payable || fee != c.fee {
			t.Errorf("手续费 %d bps + %d 分：%d -> %d/%d，期望 %d/%d", c.feeBPS, c.feeFixed, c.amount, payable, fee, c.payable, c.fee)
		}
	}
}
func TestChannelCryptoQuoteFollowsConfiguredRate(t *testing.T) {
	channel := Channel{CryptoCurrency: "USDT", Rate: "7.25"}
	// 101.50 元 ÷ 7.25 = 14.00 USDT
	if got := channel.CryptoQuote(10150); got != "14.00" {
		t.Fatalf("按汇率折算不对: %s", got)
	}
	// 除不尽时向上取整到分：报价宁可多一分。
	if got := channel.CryptoQuote(10151); got != "14.01" {
		t.Fatalf("向上取整不对: %s", got)
	}
	// 没配币种或汇率就不报价，而不是当成 1:1。
	if got := (Channel{Rate: "7.25"}).CryptoQuote(10000); got != "" {
		t.Fatalf("缺少币种时仍然报价: %s", got)
	}
	if got := (Channel{CryptoCurrency: "USDT"}).CryptoQuote(10000); got != "" {
		t.Fatalf("缺少汇率时仍然报价: %s", got)
	}
	if got := (Channel{CryptoCurrency: "USDT", Rate: "0"}).CryptoQuote(10000); got != "" {
		t.Fatalf("零汇率时仍然报价: %s", got)
	}
}

// 通道手续费只改变付给网关的金额，钱包到账的仍是用户要求的充值额。
func TestOrderFeeIsChargedToGatewayButNotCredited(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	adapter := &countingAdapter{}
	channel := Channel{Adapter: adapter, NotifyURL: "https://panel.invalid/notify", ReturnURL: "https://panel.invalid/", FeeBPS: 200, FeeFixed: 100, CryptoCurrency: "USDT", Rate: "7.2"}
	order, err := s.CreateAdapterOrder(ctx, "alice", "tokenpay", "topup", 10000, channel, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if order.Amount != 10000 || order.Payable != 10300 || order.Fee != 300 {
		t.Fatalf("订单金额拆分不对: %+v", order)
	}
	if adapter.status.AmountCents != 10300 {
		t.Fatalf("网关收到的不是实付金额: %d", adapter.status.AmountCents)
	}
	if order.PayableCrypto != "14.31" { // 103.00 ÷ 7.2 向上取整到分
		t.Fatalf("加密报价不对: %s", order.PayableCrypto)
	}
	if _, err := s.ReconcileOrder(ctx, "alice", order.ID, map[string]Channel{"tokenpay": channel}); err != nil {
		t.Fatal(err)
	}
	wallet, err := s.Wallet(ctx, "alice")
	if err != nil || wallet.Balance != 10000 {
		t.Fatalf("到账金额应当等于充值额: %+v %v", wallet, err)
	}
}

// 升级前的订单没有 payable_cents，读写都必须按「实付等于到账」处理。
func TestLegacyOrderWithoutPayableStillConfirms(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	adapter := &countingAdapter{}
	channel := Channel{Adapter: adapter, NotifyURL: "https://panel.invalid/notify", ReturnURL: "https://panel.invalid/"}
	order, err := s.CreateAdapterOrder(ctx, "alice", "tokenpay", "topup", 5000, channel, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	// 直连数据库也要走 Rebind：Postgres 的占位符是 $1，裸写 ? 只在 SQLite/MySQL 上成立。
	if _, err := s.DB.ExecContext(ctx, s.q("UPDATE commerce_orders SET payable_cents=0 WHERE id=?"), order.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.ReconcileOrder(ctx, "alice", order.ID, map[string]Channel{"tokenpay": channel})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Payable != 5000 || reloaded.Fee != 0 {
		t.Fatalf("老订单的实付金额应当回退到到账金额: %+v", reloaded)
	}
	wallet, err := s.Wallet(ctx, "alice")
	if err != nil || wallet.Balance != 5000 {
		t.Fatalf("老订单入账不对: %+v %v", wallet, err)
	}
}
