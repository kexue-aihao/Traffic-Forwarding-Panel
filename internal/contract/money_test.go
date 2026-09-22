package contract

import "testing"

func TestParseAmountUsesYuanAndRejectsAmbiguity(t *testing.T) {
	cases := []struct {
		in     string
		cents  int64
		broken bool
	}{
		{in: "1", cents: 100},
		{in: "1.5", cents: 150},
		{in: "100.00", cents: 10000},
		{in: " 12.34 ", cents: 1234},
		{in: "0.01", cents: 1},
		{in: "10000000000.00", cents: MaxAmountCents},
		{in: "", broken: true},
		{in: "1.234", broken: true},
		{in: "-1", broken: true},
		{in: "1e3", broken: true},
		{in: "一千", broken: true},
		{in: "1,000", broken: true},
		{in: ".5", broken: true},
		{in: "10000000000000", broken: true},
	}
	for _, c := range cases {
		cents, err := ParseAmount(c.in)
		if c.broken {
			if err == nil {
				t.Errorf("非法金额被接受: %q -> %d", c.in, cents)
			}
			continue
		}
		if err != nil || cents != c.cents {
			t.Errorf("金额解析不对: %q -> %d (%v)", c.in, cents, err)
		}
		round, roundErr := ParseAmount(FormatAmount(cents))
		if roundErr != nil || round != cents {
			t.Errorf("往返丢精度: %d -> %s -> %d (%v)", cents, FormatAmount(cents), round, roundErr)
		}
	}
}

// 站点设置以元为单位；旧文档里只有按分写的老字段，读回来要等价。
func TestSiteSettingsNormalizeUpgradesCentFields(t *testing.T) {
	legacy := SiteSettings{Name: "站点", LegacyMinimumRechargeCents: 500, LegacyMaximumRechargeCents: 100000}
	legacy.Normalize()
	if legacy.MinimumRecharge != "5.00" || legacy.MaximumRecharge != "1000.00" {
		t.Fatalf("旧的分字段没有换算成元: %+v", legacy)
	}
	if legacy.Currency != SettlementCurrency {
		t.Fatalf("结算币种没有补齐: %q", legacy.Currency)
	}
	// 换算之后旧字段不再回写，避免同一个金额出现两种单位。
	if legacy.LegacyMinimumRechargeCents != 0 || legacy.LegacyMaximumRechargeCents != 0 {
		t.Fatal("旧字段被保留下来了")
	}
	min, max, err := legacy.RechargeRange()
	if err != nil || min != 500 || max != 100000 {
		t.Fatalf("区间还原不对: %d %d %v", min, max, err)
	}

	empty := SiteSettings{}
	empty.Normalize()
	if empty.MinimumRecharge != "1.00" || empty.MaximumRecharge != "1000000.00" {
		t.Fatalf("默认值不对: %+v", empty)
	}
	broken := SiteSettings{MinimumRecharge: "abc", MaximumRecharge: "10"}
	if _, _, err := broken.RechargeRange(); err == nil {
		t.Fatal("非法区间被接受")
	}
	inverted := SiteSettings{MinimumRecharge: "10.00", MaximumRecharge: "1.00"}
	if _, _, err := inverted.RechargeRange(); err == nil {
		t.Fatal("上下限颠倒被接受")
	}
}
